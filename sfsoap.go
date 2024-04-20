package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sfsoap/config"
	"sfsoap/entities"
	"sfsoap/entities/leadvisits"
	"sfsoap/entities/salesforcerest"
	"sfsoap/entities/salesforcestream"
	"sfsoap/generator"
	"sfsoap/globals"
	"sfsoap/helpers"
	"sfsoap/helpers/clamd"
	"sfsoap/helpers/restart"
	"sfsoap/helpers/templates"
	"sfsoap/helpers/textextract"
	"sfsoap/scheduler"
	"sfsoap/solr"
	"sfsoap/switchsettings"

	_ "net/http/pprof"
)

//go:embed templates/*
var embededTpls embed.FS

var (
	RUNID              = fmt.Sprintf("%v", time.Now().UnixNano())
	generate           = ""
	skipContentLoading = false
	downloadTika       = false
	cfgFilePath        = ""
	cfg                *config.AppConfig
	solrCoreJobs       *solr.Core
	solrCoreCompanies  *solr.Core
	solrCoreCandidates *solr.Core
	tpls               *templates.Tmpl
)

func main() {
	var err error
	checkCLIParams()

	if cfgFilePath != "" {
		cfg, err = config.ParseConfigFromFile(filepath.Base(os.Args[0]), cfgFilePath)
	} else {
		log.Fatalf("Please provide a path to a config file.\nExample: -cfgFilePath xxxxx.config\n")
	}
	if err != nil {
		log.Fatal("could not ParseConfigFromFile from main", err)
	}
	reloadTemplatesAndAssets := false
	if cfg.AppMode == globals.DEV {
		reloadTemplatesAndAssets = true
	}
	loadTemplates(reloadTemplatesAndAssets)

	restart.SetupRestartListener()

	log.Println("use solr", cfg.UseSolr)
	ownName := cfg.PreviousProcessToKill
	sep := string(filepath.Separator)
	if cfg.PersistentDataDir != "" {
		cfg.PersistentDataDir = strings.TrimRight(cfg.PersistentDataDir, sep) + sep
	} else {
		cfg.PersistentDataDir = "./"
	}
	if err := helpers.IsWritable(cfg.PersistentDataDir); err != nil {
		log.Fatalf(cfg.PersistentDataDir, " (cfg.PersistentDataDir) not writeable or does not exist ", err.Error())
	}
	if cfg.CacheDir != "" {
		cfg.CacheDir = strings.TrimRight(cfg.CacheDir, sep) + sep
	} else {
		cfg.CacheDir = "./"
	}
	for _, dir := range []string{cfg.CacheDir + "/" + "account_logos", cfg.CacheDir + "/" + "account_textimages"} {
		if err := helpers.IsWritable(dir); err != nil {
			log.Fatal(cfg.CacheDir, " (cfg.CacheDir+\"/\"+dir) not writeable or does not exist ", err.Error())
		}
	}
	if cfg.LogDir != "" {
		cfg.LogDir = strings.TrimRight(cfg.LogDir, sep) + sep
	} else {
		cfg.LogDir = "./"
	}
	if err := helpers.IsWritable(cfg.LogDir); err != nil {
		log.Fatal(cfg.LogDir, " (cfg.LogDir) not writeable or does not exist ", err.Error())
	}

	if cfg.Brand == "" || (cfg.Brand != globals.ANDWORK && cfg.Brand != globals.GOODCOMPANY && cfg.Brand != globals.JOUWICT) {
		log.Fatalf("There is no 'brand' (goodcompany, andwork, jouwict) specified in the config file")
	}
	cfg.Host = strings.TrimSuffix(cfg.Host, "/")
	if strings.TrimSpace(cfg.Host) == "" {
		if cfg.Brand == globals.ANDWORK {
			cfg.Host = "https://andwork.com"
			if cfg.AppMode != globals.PROD {
				cfg.Host = "https://dev.andwork.com"
			}
		}
	}
	globals.SetConfig(globals.Config{
		Brand:             cfg.Brand,
		AppMode:           cfg.AppMode,
		LogDir:            cfg.LogDir,
		PersistentDataDir: cfg.PersistentDataDir,
		CacheDir:          cfg.CacheDir,
	})
	clamd.SetConfig(clamd.Config{
		ClamdConnectionString: cfg.ClamdConnectionString,
	})
	textextract.SetCondig(textextract.Config{
		JavaBinPath:         cfg.JavaBinPath,
		TikaJarPath:         cfg.TikaJarPath,
		TikaOCRConfigPath:   cfg.TikaOCRConfigPath,
		TikaNOOCRConfigPath: cfg.TikaNOOCRConfigPath,
	})
	if err := leadvisits.SetConfig(leadvisits.Config{
		Brand:             cfg.Brand,
		AppMode:           cfg.AppMode,
		PersistentDataDir: cfg.PersistentDataDir,
		LogDir:            cfg.LogDir,
	}); err != nil {
		log.Fatal(err)
	}

	switchsettings.SetConfig(switchsettings.Config{
		PersistentDataDir: cfg.PersistentDataDir,
	})
	entities.SetConfig(entities.Config{
		Brand:                   cfg.Brand,
		AppMode:                 cfg.AppMode,
		LogDir:                  cfg.LogDir,
		PersistentDataDir:       cfg.PersistentDataDir,
		CacheDir:                cfg.CacheDir,
		EventURL:                cfg.EventURL,
		UseSolr:                 cfg.UseSolr,
		RemoteFileStorageServer: cfg.RemoteFileStorageServer,
	})
	salesforcerest.SetCredentials(salesforcerest.Credentials{
		Username:       cfg.Username,
		Password:       cfg.Password,
		SecurityToken:  cfg.SecurityToken,
		ConsumerKey:    cfg.ConsumerKey,
		ConsumerSecret: cfg.ConsumerSecret,
		URLLogin:       cfg.URLLoginREST,
		EndpointURI:    cfg.EndpointURIREST,
	})
	sfrestConfig := salesforcerest.Config{
		JWTLoginPrivateKeyPath: cfg.JWTLoginPrivateKeyPath,
		AppMode:                cfg.AppMode,
		LogDir:                 cfg.LogDir,
		Retries:                1,
	}
	if cfg.AppMode != globals.PROD {
		sfrestConfig.Retries = 3
	}
	salesforcerest.SetConfig(sfrestConfig)

	salesforcestream.SetConfig(salesforcestream.Config{
		AppMode:           cfg.AppMode,
		PersistentDataDir: cfg.PersistentDataDir,
	})

	if cfg.UseSolr {
		solr.SetConfig(solr.Config{
			PersistentDataDir: cfg.PersistentDataDir,
		})
		solrCoreJobs, err = solr.InitCore(cfg.SolrBaseURL, strings.ToLower(string(cfg.Brand)+"-jobs"), "", entities.SolrDocumentJob{})
		if err != nil {
			log.Fatal("InitCore jobs in main", err)
		}
		if cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT {
			solrCoreCompanies, err = solr.InitCore(cfg.SolrBaseURL, strings.ToLower(string(cfg.Brand)+"-companies"), "", entities.SolrDocumentCompany{})
			if err != nil {
				log.Fatal("InitCore companies in main", err)
			}
			if cfg.UseSolrCandidate {
				solrCoreCandidates, err = solr.InitCore(cfg.SolrBaseURL, strings.ToLower(string(cfg.Brand)+"-candidates"), "", entities.SolrDocumentCandidate{})
				if err != nil {
					log.Fatal("InitCore candidates in main", err)
				}
			}
		}
		entities.UpdateSolr = updateSolr
	}
	if generate != "" {
		err := generator.Generate(generate)
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	start := time.Now()
	if cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT || (cfg.Brand == globals.GOODCOMPANY && cfg.AppMode != globals.PROD && cfg.DbName != "") {
		initDatabase(cfg)
	}
	start = printDuration("initDB", start)

	lastSoldPackageCreatedTime, err := entities.RetrieveSoldPackageLastCreatedDate()
	if err != nil {
		log.Fatal(err)
	}
	entities.CachedSoldPackages.SetLastCreatedDate(*lastSoldPackageCreatedTime)
	log.Printf("Set last sold package created time on %v", lastSoldPackageCreatedTime)
	start = printDuration("Find last package created date", start)

	chLeadAccounts := make(chan bool)
	go func() {
		entities.CachedAccountsForLeads.UpdateAll()
		chLeadAccounts <- true
	}()
	chOpenLeads := make(chan bool)
	go func() {
		entities.CachedOpenLeads.UpdateAll()
		chOpenLeads <- true
	}()

	start = printDuration("leadfeeder accounts", start)

	log.Println("Fetching labels, jobs, accounts and recruiters... one moment please")
	if err = salesforcerest.RetreiveAllObjectFields(); err != nil {
		log.Fatalf("Error getting object fields: %v", err)
	}
	start = printDuration("Object fields", start)

	if !skipContentLoading {

		jobLabelgroups, jobLabelgroupsByKey, jobLabelsByGroupKeyAndID, jobLabelsByGroupKeyAndValue, jobLabelsByGroupKeyAndPerma, err := entities.GetAndMakeLabels("position")
		if err != nil {
			log.Fatalf("Error making job labels: %v\n", err)
		}
		entities.CachedJobs.UpdateLabelgroups(jobLabelgroups, jobLabelgroupsByKey, jobLabelsByGroupKeyAndID, jobLabelsByGroupKeyAndValue, jobLabelsByGroupKeyAndPerma)
		start = printDuration("Labelgroups Jobs", start)

		companyLabelgroups, companyLabelgroupsByKey, companyLabelsByGroupKeyAndID, companyLabelsByGroupKeyAndValue, companyLabelsByGroupKeyAndPerma, err := entities.GetAndMakeLabels("account")
		if err != nil {
			log.Fatalf("Error making company labels: %v\n", err)
		}
		entities.CachedCompanies.UpdateLabelgroups(companyLabelgroups, companyLabelgroupsByKey, companyLabelsByGroupKeyAndID, companyLabelsByGroupKeyAndValue, companyLabelsByGroupKeyAndPerma)
		start = printDuration("Labelgroups comps", start)

		recruiters, err := entities.RetrieveContactsRecruiters()
		if err != nil {
			log.Fatalf("Error getting recruiters: %v\n", err)
		}
		entities.CachedContacts.UpdateAllRecruiters(recruiters)
		printDuration("Recruiters", start)

		if _, err := entities.MakePublishedJobsAndAccountsFromJobIDs(nil, false); err != nil {
			log.Println("main MakePublishedJobs failed (continuing) ", err)
		}
		log.Println("Jobs converted and cached")

		if cfg.Brand == globals.JOUWICT || cfg.Brand == globals.ANDWORK {
			go func() {
				time.Sleep(10 * time.Second)
				log.Println("Getting steps of active Jobs")
				jobIDs := []string{}
				for _, job := range entities.CachedJobs.GetAll(true) {
					jobIDs = append(jobIDs, job.Full.ID)
				}
				steps, err := entities.RetrieveStepsFrom(entities.SOBJ_Position, jobIDs)
				if err != nil {
					log.Printf("Error getting steps: %s", err)
				} else {
					entities.CachedSteps.Add(steps)
				}
				log.Println("Getting steps of active Jobs done!")
			}()
		}
		if cfg.UseSolr {
			solrJobs := entities.CachedJobs.GetAllSolrDocuments()
			entities.CachedSolrJobs.Update(solrJobs, true)
			if err := solrCoreJobs.UpdateDocs(solrJobs); err != nil {
				log.Println("jobs UpdateDocs in main", err)
			}
			if cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT {
				companies := entities.CachedCompanies.GetAllSolrDocuments()
				entities.CachedSolrCompanies.Update(companies, true)
				if err := solrCoreCompanies.UpdateDocs(companies); err != nil {
					log.Println("companies UpdateDocs in main", err)
				}
			}
		}
		// go updateAllJobsOnTimer()
		go updateLabelGroupsCountsOnTimer()
	}
	scheduler.SetConfig(scheduler.Config{
		PersistentDataDir: cfg.PersistentDataDir,
	})
	err = writeStartFile()
	if err != nil {
		log.Println("start file in main", err)
	}

	//go entities.ProcessJobApplicationQueue()
	go entities.ListenStreamingAPI()
	if cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT {
		if cfg.AppMode == globals.PROD {
			startSendEmailsTimer()
		}
	}

	// Wait for parallel routines
	<-chLeadAccounts
	close(chLeadAccounts)
	<-chOpenLeads
	close(chOpenLeads)

	go processLeadVisitsOnTimer()

	// go func() {
	// 	for {
	// 		ids, err := entities.RetrieveJobApplicationIDsOfNotSendEmailsInLastWeek()
	// 		if err != nil {
	// 			log.Println(err)
	// 			time.Sleep(10 * time.Minute)
	// 			continue
	// 		}
	// 		jobAppIdsWithStatusses := map[string]*entities.QueueItemJobApplicationCreate{}
	// 		for _, id := range ids {
	// 			jobAppIdsWithStatusses[id] = &entities.QueueItemJobApplicationCreate{
	// 				JobApplicationID: id,
	// 				Status:           "manually triggered email",
	// 				Action:           "all 'non send' of last week",
	// 			}
	// 		}
	// 		err = entities.HandleNewJobApplications(jobAppIdsWithStatusses)
	// 		if err != nil {
	// 			log.Println(err)
	// 			time.Sleep(10 * time.Minute)
	// 			continue
	// 		}
	// 		log.Println("Manual send emails done")

	// 		time.Sleep(30 * time.Minute)
	// 	}
	// }()
	setRoutes()
	startWebServer(ownName)
}
func loadTemplates(reload bool) {
	tpls = templates.NewTmpl(embededTpls, "templates", "templates", ".gohtml", reload, template.FuncMap{
		"replace_in_str": func(input string, from string, to interface{}) string {
			return strings.Replace(input, from, fmt.Sprintf("%v", to), -1)
		},
		"replace_in_html": func(input template.HTML, from string, to interface{}) template.HTML {
			return template.HTML(strings.Replace(string(input), from, fmt.Sprintf("%v", to), -1))
		},
		"replace_in_js": func(input template.JS, from string, to interface{}) template.JS {
			return template.JS(strings.Replace(string(input), from, fmt.Sprintf("%v", to), -1))
		},
		"call_template": func(name string, data interface{}) (ret template.HTML, err error) {
			buf := bytes.NewBuffer([]byte{})
			if err := tpls.Render(buf, name, data); err != nil {
				return "", err
			}
			ret = template.HTML(buf.String())
			return
		},
		"runid": func() template.JS {
			return template.JS(RUNID)
		},
	})
}
func updateLabelGroupsCountsOnTimer() {
	t := time.Now()
	n := time.Date(t.Year(), t.Month(), t.Day()+1, 3, 0, 0, 0, t.Location())
	d := n.Sub(t)
	if d < 0 {
		n = n.Add(24 * time.Hour)
		d = n.Sub(t)
	}
	for {
		time.Sleep(d)
		d = 24 * time.Hour
		entities.UpdateLabelGroupsCountsJobs()
		entities.UpdateLabelGroupsCountsCompanies()
	}
}
func updateAllJobsOnTimer() {
	for range time.Tick(15 * time.Minute) {
		//log.Println("run update all jobs")
		recruiters, err := entities.RetrieveContactsRecruiters()
		if err == nil {
			entities.CachedContacts.UpdateAllRecruiters(recruiters)
		} else {
			log.Println("updateAllJobsOnTimer RetrieveContactsRecruiters", err)
		}
		if _, err := entities.MakePublishedJobsAndAccountsFromJobIDs(nil, true); err != nil {
			log.Println("updateAllJobsOnTimer MakePublishedJobs failed (continuing) ", err)
		} else {
			if cfg.UseSolr {
				solrJobs := entities.CachedJobs.GetAllSolrDocuments()
				entities.CachedSolrJobs.Update(solrJobs, true)
				if err := solrCoreJobs.UpdateDocs(solrJobs); err != nil {
					log.Println(err)
				}

				if cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT {
					companies := entities.CachedCompanies.GetAllSolrDocuments()
					entities.CachedSolrCompanies.Update(companies, true)
					if err := solrCoreCompanies.UpdateDocs(companies); err != nil {
						log.Println(err)
					}
				}
			}
		}
	}
}
func processLeadVisitsOnTimer() {
	failMatch := .4
	goodMatch := .9
	for range time.Tick(4 * time.Hour) {
		if switchsettings.Settings.LeadfeederDisabled {
			return
		}
		te := time.Now()
		year, month, day := te.Date()
		ts := time.Date(year, month, day, 0, 0, 0, 0, te.Location())

		visits, err := leadvisits.GetLeadfeederVisits(ts, te)
		if err != nil {
			log.Println(err)
		} else {
			parseResponse, err := leadvisits.ParseVisits(visits, failMatch, goodMatch)
			if err != nil {
				log.Println(err)
			} else {
				actions := leadvisits.CommitParsedVisitResults(parseResponse)
				for _, action := range actions {
					if !strings.Contains(action, "SUCCESS") {
						log.Println(action)
					}
				}
			}
		}

		visits, err = leadvisits.GetLeadinfoVisits()
		if err != nil {
			log.Println(err)
		} else {
			parseResponse, err := leadvisits.ParseVisits(visits, failMatch, goodMatch)
			if err != nil {
				log.Println(err)
			} else {
				actions := leadvisits.CommitParsedVisitResults(parseResponse)
				for _, action := range actions {
					if !strings.Contains(action, "SUCCESS") {
						log.Println(action)
					}
				}
			}
		}
	}
}
func writeStartFile() error {
	d1 := []byte("positions and labels received and ready...")
	startFileFilename, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot get home dir for start file: %v", err)
	}
	startFileFilename += string(filepath.Separator) + "sfgo-started"
	err = os.WriteFile(startFileFilename, d1, 0640)
	if err != nil {
		return fmt.Errorf("cannot write start file: %s ; %v", startFileFilename, err)
	}
	return nil
}
func printDuration(lbl string, start time.Time) time.Time {
	log.Println(lbl, time.Since(start).Milliseconds(), "ms")
	return time.Now()
}
