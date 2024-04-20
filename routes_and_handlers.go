package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"sfsoap/entities"
	"sfsoap/entities/googlegeocode"
	"sfsoap/entities/leadvisits"
	"sfsoap/entities/salesforcerest"
	"sfsoap/entities/salesforcestream"
	"sfsoap/geowhois"
	"sfsoap/globals"
	"sfsoap/helpers"
	"sfsoap/helpers/levenshtein"
	"sfsoap/scheduler"
	"sfsoap/sfids"
	"sfsoap/solr"
	"sfsoap/switchsettings"
)

type openConnTracker struct {
	aMux        sync.Mutex
	connections map[string]uint32
}
type shouldWriteDangerTracker struct {
	aMux          sync.Mutex
	shouldWrite10 bool
	shouldWrite20 bool
}

func (s *shouldWriteDangerTracker) set10(v bool) {
	s.aMux.Lock()
	defer s.aMux.Unlock()
	s.shouldWrite10 = v
}
func (s *shouldWriteDangerTracker) set20(v bool) {
	s.aMux.Lock()
	defer s.aMux.Unlock()
	s.shouldWrite20 = v
}
func (s *shouldWriteDangerTracker) get10() bool {
	s.aMux.Lock()
	defer s.aMux.Unlock()
	return s.shouldWrite10
}
func (s *shouldWriteDangerTracker) get20() bool {
	s.aMux.Lock()
	defer s.aMux.Unlock()
	return s.shouldWrite20
}

var (
	conns             = openConnTracker{connections: map[string]uint32{}}
	dangerShouldWrite = shouldWriteDangerTracker{
		shouldWrite10: true,
		shouldWrite20: true,
	}
)

const fNameOpenConnections = "open_connections.log"

func baseHandler(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func(w http.ResponseWriter, r *http.Request) {
			_ = r.Body.Close()
			if rec := recover(); rec != nil {
				JSONError(w, r, fmt.Errorf("runtime error: %#v", rec), http.StatusInternalServerError, true, true)
			}
		}(w, r)
		cpy := map[string]uint32{}
		conns.aMux.Lock()
		conns.connections[route] += 1
		for k, v := range conns.connections {
			cpy[k] = v
		}
		conns.aMux.Unlock()

		var totalConns uint32 = 0
		output := strings.Builder{}
		output.Write([]byte(time.Now().String() + "\n"))
		for k, v := range cpy {
			if v > 0 {
				_, _ = output.WriteString(strconv.Itoa(int(v)) + ": " + k + "\n")
				totalConns += v
			}
		}
		if f, err := os.CreateTemp("/tmp", "sfsoap-oc-*"); err != nil {
			log.Printf("Open connections (/tmp/sfsoap-oc-*) write error: %v\n", err)
		} else {
			name := f.Name()
			_, _ = f.WriteString(output.String())
			_ = f.Close()
			if err = os.Rename(name, cfg.LogDir+fNameOpenConnections); err != nil {
				if err = os.Remove(name); err != nil {
					log.Println("cannot remove ", name, " from /tmp", err)
				}
			}
		}
		if totalConns > 80 {
			log.Println("quiting daemon because number of connections exceeds 80", totalConns)
			quitDaemon <- 1
		} else if totalConns >= 20 && dangerShouldWrite.get20() {
			_ = writeConnLogDangerFile(cfg.LogDir, "sfsoap-oc-20-*", []byte(output.String()))
			dangerShouldWrite.set20(false)
		} else if totalConns >= 10 && dangerShouldWrite.get10() {
			_ = writeConnLogDangerFile(cfg.LogDir, "sfsoap-oc-5-*", []byte(output.String()))
			dangerShouldWrite.set10(false)
		} else if totalConns < 10 {
			dangerShouldWrite.set10(true)
			dangerShouldWrite.set20(true)
		}

		next.ServeHTTP(w, r)

		conns.aMux.Lock()
		conns.connections[route] -= 1
		conns.aMux.Unlock()
	}
}
func writeConnLogDangerFile(dir string, pattern string, data []byte) error {
	f, err := os.CreateTemp(dir, pattern)
	defer func() { _ = f.Close() }()
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func setRoutes() {
	conns.connections = make(map[string]uint32, 40)

	http.HandleFunc("/int/admin/sf/ja", baseHandler("/int/admin/ja", func(w http.ResponseWriter, r *http.Request) {

		if r.Method == http.MethodPost {

			var postData entities.JobApplicationCreateInput
			err := json.NewDecoder(r.Body).Decode(&postData)
			if err != nil {
				JSONError(w, r, err, http.StatusInternalServerError, true, false)
				return
			}
			resultData, err := entities.CreateJobApplication(postData)
			if err != nil {
				if err.Error() == "validation_errors" {
					JSONOutput(w, resultData)
					return
				}
				JSONError(w, r, err, http.StatusInternalServerError, true, false)
				return
			}
			JSONOutput(w, resultData)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		err := tpls.Render(w, "jobapplication", struct {
			Types []string
		}{
			Types: entities.JobApplicstionTypes,
		})
		if err != nil {
			log.Println(err)
		}
	}))
	http.HandleFunc("/int/admin/sf/ja/cvupload", baseHandler("/int/admin/ja/cvupload", func(w http.ResponseWriter, r *http.Request) {
		name, err := entities.UploadCandidateCV(r)
		if err != nil {
			log.Println(err)
			outErr := fmt.Errorf("an unknown error occured, please contact us")
			if strings.Contains(err.Error(), "too large") {
				outErr = fmt.Errorf("file too big (5mb limit)")
			} else if strings.Contains(err.Error(), "not a valid mime") {
				outErr = fmt.Errorf("only pdf, docx or doc files allowed")
			}
			JSONError(w, r, outErr, http.StatusInternalServerError, true, false)
			return
		}
		JSONOutput(w, map[string]string{
			"file": name,
		})
	}))
	http.HandleFunc("/testJA/", baseHandler("/testJA/", func(w http.ResponseWriter, r *http.Request) {
		// open := entities.JobApplicationCreateUpdate{
		// 	Name:             "{Fullname} (open)",
		// 	RecordTypeID:     "0122p000000R7fxAAC",
		// 	WorkflowStatusID: "a182p00001MMFlaAAH",
		// SourceOfJobApplication: "Website",
		// 	CandidateID:      "a0A2p00001FZmacEAD",
		// 	Motivation:       "test",
		// }
		normal := entities.JobApplicationCreateUpdate{
			CandidateID:            "a0A2p00001FZmacEAD",
			PositionID:             "a0wAb0000008JWwIAM",
			SourceOfJobApplication: "Website",
			Motivation:             "test",
		}

		jaID, err := entities.CreateJobApplicationForCandidateID(normal)
		//a0eG500000043tlIAA
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		JSONOutput(w, map[string]string{"JobApplicationID": jaID})
	}))
	// http.HandleFunc("/candidatecvs/", baseHandler("/candidatecvs/", func(w http.ResponseWriter, r *http.Request) {
	// 	fragments := helpers.GetFragmentsAfter("candidatecvs", r.URL.Path)
	// 	cvdata, err := entities.DownloadAllCVsFromCandidate(fragments[0])
	// 	if err != nil {
	// 		JSONError(w, r, err, http.StatusInternalServerError,false, false)
	// 		return
	// 	}
	// 	JSONOutput(w, cvdata)
	// }))
	http.HandleFunc("/testtoremote/", baseHandler("/testtoremote/", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()

		fpath := filepath.Join(globals.CacheDirs.VirusScan, "test.doc")
		filename := "test.doc"
		classOfFile := string(globals.FS_CV)
		createdAt := &now
		virusScannedAt := &now

		resp, err := entities.WriteToRemoteFileStorage(fpath, filename, classOfFile, createdAt, virusScannedAt, false)

		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		response := fmt.Sprintf("We have response: %s", helpers.PrettyPrintReturn(resp))
		_, _ = w.Write([]byte(response))
	}))

	http.HandleFunc("/favicon.ico", baseHandler("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {}))
	http.HandleFunc("/Labels", baseHandler("/Labels", handleLabels))
	http.HandleFunc("/Labels/ForTrans", baseHandler("/Labels/ForTrans", handleLabelsForTrans))

	if cfg.UseSolr {
		http.HandleFunc("/Jobs/Search", baseHandler("/Jobs/Search", jobsSearchHandler))
		http.HandleFunc("/Jobs/Search/", baseHandler("/Jobs/Search/", jobsSearchHandler))
		http.HandleFunc("/Job/Published/Solr/", baseHandler("/Job/Published/Solr/", handlePublishedJobsIDSolr))
		http.HandleFunc("/Jobs/Feed/Solr", baseHandler("/Jobs/Feed/Solr", handleJobsFeedSolr))
		http.HandleFunc("/Companies/Search", baseHandler("/Companies/Search", companiesSearchHandler))
		http.HandleFunc("/Companies/Search/", baseHandler("/Companies/Search/", companiesSearchHandler))
		http.HandleFunc("/Company/Published/Solr/", baseHandler("/Company/Published/Solr/", handlePublishedCompaniesIDSolr))
		http.HandleFunc("/Companies/Feed/Solr", baseHandler("/Companies/Feed", handleCompaniesFeedSolr))
		http.HandleFunc("/Candidates/Search", baseHandler("/Candidates/Search", candidatesSearchHandler))
		http.HandleFunc("/Candidates/Search/", baseHandler("/Candidates/Search/", candidatesSearchHandler))
		http.HandleFunc("/int/staff/Jobs/Feed/Solr", baseHandler("/Jobs/Feed/Solr", handleJobsFeedSolr))
		http.HandleFunc("/int/srv/Jobs/Feed/Solr", baseHandler("/Jobs/Feed/Solr", handleJobsFeedSolr))
	}
	http.HandleFunc("/Jobs/Feed", baseHandler("/Jobs/Feed", handleJobsFeed))
	http.HandleFunc("/Jobs/Feed/Version", baseHandler("/Jobs/Feed/Version", handleJobsFeedVersion))
	http.HandleFunc("/int/srv/Jobs/Feed/Version", baseHandler("/Jobs/Feed/Version", handleJobsFeedVersion))

	http.HandleFunc("/Job/Published/", baseHandler("/Job/Published/", handlePublishedJobsID))
	http.HandleFunc("/Job/URLBy4NetID/", baseHandler("/Job/URLBy4NetID/", handleJobURLBy4NetID))
	http.HandleFunc("/Job/URLBy4NetPerma/", baseHandler("/Job/URLBy4NetPerma/", handleJobURLBy4NetPerma))

	http.HandleFunc("/Companies/Feed", baseHandler("/Companies/Feed", handleCompaniesFeed))
	http.HandleFunc("/Companies/Feed/Version", baseHandler("/Companies/Feed/Version", handleCompaniesFeedVersion))
	http.HandleFunc("/Company/Logo/", baseHandler("/Company/Logo/", handleCompanyLogo))
	http.HandleFunc("/Company/TextImage/", baseHandler("/Company/TextImage/", handleCompanyTextImage))

	http.HandleFunc("/Company/Published/", baseHandler("/Company/Published/", handlePublishedCompaniesID))
	http.HandleFunc("/Company/All/", baseHandler("/Company/All/", handleAllCompaniesID))

	http.HandleFunc("/Company/JobApplications/", baseHandler("/Company/JobApplications/", handleCompanyJobApplications))
	http.HandleFunc("/Company/Jobs/", baseHandler("/Company/Jobs/", handleCompanyJobs))
	http.HandleFunc("/Company/URLBy4NetID/", baseHandler("/Company/URLBy4NetID/", handleCompanyURLBy4NetID))
	http.HandleFunc("/Company/URLBy4NetPerma/", baseHandler("/Company/URLBy4NetPerma/", handleCompanyURLBy4NetPerma))
	http.HandleFunc("/Company/", baseHandler("/Company/", handleCompaniesID))

	http.HandleFunc("/Candidate/ByEmail", baseHandler("/Candidate/ByEmail", handleCandidateByEmail))
	http.HandleFunc("/Candidate/ByEmailAndPassword", baseHandler("/Candidate/ByEmailAndPassword", handleCandidateByEmailAndPassword))
	http.HandleFunc("/Candidate/Status/ByID/", baseHandler("/Candidate/Status/ByID/", handleCandidateStatusByID))
	http.HandleFunc("/Candidate/Status", baseHandler("/Candidate/Status", handleCandidateStatus)) // deprecated
	http.HandleFunc("/Candidate/ValidateEmail", baseHandler("/Candidate/ValidateEmail", handleCandidateValidateEmail))
	http.HandleFunc("/Candidate/FinishRegistration", baseHandler("/Candidate/FinishRegistration", handleCandidateFinishRegistration))
	http.HandleFunc("/Candidate/RequestPasswordReset", baseHandler("/Candidate/RequestPasswordReset", handleCandidateRequestPasswordReset))
	http.HandleFunc("/Candidate/ResetPassword", baseHandler("/Candidate/ResetPassword", handleCandidateResetPassword))
	http.HandleFunc("/Candidate/UpdatePassword/", baseHandler("/Candidate/UpdatePassword/", handleCandidateUpdatePassword))
	http.HandleFunc("/Candidate/JobApplications", baseHandler("/Candidate/JobApplications", handleCandidateJobApplications))
	http.HandleFunc("/Candidate/UpdateDrivers/", baseHandler("/Candidate/UpdateDrivers/", handleCandidateUpdateDrivers))
	http.HandleFunc("/Candidate/Update/", baseHandler("/Candidate/Update/", handleCandidateUpdate))
	http.HandleFunc("/Candidate/UpdateCV/", baseHandler("/Candidate/UpdateCV/", handleCandidateUpdateCV))

	http.HandleFunc("/JobApplication/AcceptRejectURLBy4NetGUID/", baseHandler("/JobApplication/AcceptRejectURLBy4NetGUID/", handleJobApplicationAcceptRejectURLBy4NetGUID))
	http.HandleFunc("/JobApplication/CV/Test/Hash/", baseHandler("/JobApplication/CV/Test/Hash/", handleJobApplicationCVTestHash))
	http.HandleFunc("/JobApplication/CV/Auth/ReHash/", baseHandler("/JobApplication/CV/Auth/ReHash/", handleJobApplicationCVAuthReHash))
	http.HandleFunc("/JobApplication/CV/", baseHandler("/JobApplication/CV/", handleJobApplicationCV))
	http.HandleFunc("/JobApplication/CVExists/", baseHandler("/JobApplication/CVExists/", handleJobApplicationCVExists))
	//http.HandleFunc("/JobApplication/Queue", baseHandler("/JobApplication/Queue", handleJobApplicationQueue))
	http.HandleFunc("/JobApplication/Invite/", baseHandler("/JobApplication/Invite/", handleJobApplicationInvite))
	http.HandleFunc("/JobApplication/Reject/", baseHandler("/JobApplication/Reject/", handleJobApplicationReject))
	http.HandleFunc("/JobApplication/AlreadyInvited/", baseHandler("/JobApplication/AlreadyInvited/", handleJobApplicationAlreadyInvited))
	http.HandleFunc("/JobApplication/", baseHandler("/JobApplication/", handleJobApplicationByID))

	http.HandleFunc("/Lead", baseHandler("/Lead", handleCreateLead))

	http.HandleFunc("/CustomerPortal/ContactLogin", baseHandler("/CustomerPortal/ContactLogin", handleCustomerPortalContactLogin))

	///int/srv/ ; /int/staff/ ; /int/admin/ en /ext/

	http.HandleFunc("/int/staff/sf/Jobs/Feed", baseHandler("/int/staff/sf/Jobs/Feed", handleJobsFeed))
	http.HandleFunc("/int/staff/sf/Jobs/Errors", baseHandler("/int/staff/sf/Jobs/Errors", handleJobsErrors))
	http.HandleFunc("/int/staff/sf/Jobs/Errors/Table", baseHandler("/int/staff/sf/Jobs/Errors/Table", handleJobsErrors))
	http.HandleFunc("/int/staff/sf/Jobs/Validate/Addresses", baseHandler("/int/staff/sf/Jobs/Validate/Addresses", handleJobsAddresses))
	http.HandleFunc("/int/staff/sf/LeadVisits/", baseHandler("/int/staff/sf/LeadVisits/", handleLeadVisits))
	http.HandleFunc("/int/staff/sf/JobData/", baseHandler("/int/staff/sf/JobData/", handleJobData))
	http.HandleFunc("/int/staff/sf/Counts/", baseHandler("/int/staff/sf/Counts/", handleCounts))
	http.HandleFunc("/int/staff/sf/UserPhoto/", baseHandler("/int/staff/sf/UserPhoto/", handleUserPhoto))
	http.HandleFunc("/int/admin/sf/Scheduler", baseHandler("/int/admin/sf/Scheduler", handleScheduler))
	http.HandleFunc("/int/admin/sf/ActiveUsers", baseHandler("/int/admin/sf/ActiveUsers", handleActiveUsers))
	http.HandleFunc("/int/admin/sf/TodaysOfficeLoggedinUserIDs", baseHandler("/int/admin/sf/TodayLoggedinUserIDs", handleTodaysOfficeLoggedinUserIDs))
	http.HandleFunc("/int/admin/sf/Jobs/ExternalIDs", baseHandler("/int/admin/sf/Jobs/ExternalIDs", handleJobsExternalIDs))
	//http.HandleFunc("/int/admin/JobApplication/Queue", baseHandler("/int/admin/JobApplication/Queue", handleJobApplicationQueue))
	http.HandleFunc("/int/admin/sf/JobApplication/RunItem/Demian/Master/", baseHandler("/int/admin/sf/JobApplication/RunItem/Demian/Master/", handleRunJobappManually))
	http.HandleFunc("/int/admin/JobApplication/", baseHandler("/int/admin/JobApplication/", handleJobApplicationByID))
	http.HandleFunc("/int/admin/sf/JobApplications/Reschedule/NonSendEmails", baseHandler("/int/admin/sf/JobApplications/Reschedule/NonSendEmails", handleRescheduleNonsendJobApplicationEmails))

	http.HandleFunc("/int/admin/sf/Status", baseHandler("/int/admin/sf/Status", handleStatus))
	http.HandleFunc("/int/admin/sf/Refresh/Bart/True", baseHandler("/int/admin/sf/Refresh/Bart/True", handleRefresh))
	http.HandleFunc("/int/admin/sf/StreamMessages", baseHandler("/int/admin/sf/StreamMessages", handleStreamMessages))
	http.HandleFunc("/int/admin/sf/StreamEventHistory", baseHandler("/int/admin/sf/StreamMessages", handleStreamEventHistory))
	http.HandleFunc("/int/admin/sf/AllAddresses", baseHandler("/int/admin/sf/AllAddresses", handleAllAddresses))
	http.HandleFunc("/int/admin/sf/candidateidssync", baseHandler("/int/admin/sf/candidateidssync", candidateIDsSync))
	http.HandleFunc("/int/admin/sf/cvsync", baseHandler("/int/admin/sf/cvsync", cvSync))
	http.HandleFunc("/int/admin/sf/cvsync-data", baseHandler("/int/admin/sf/cvsync-data", cvSyncData))
	http.HandleFunc("/int/admin/sf/cvsync-processbatch", baseHandler("/int/admin/sf/cvsync-processbatch", processBatchOfCandidatesForCVSync))
	http.HandleFunc("/int/admin/sf/cvsync-isrunning", baseHandler("/int/admin/sf/cvsync-isrunning", isRunningBatchOfCandidatesForCVSync))
	http.HandleFunc("/int/admin/sf/cvsync-stop-clear", baseHandler("/int/admin/sf/cvsync-stop-clear", stopAndClearCVSync))
	http.HandleFunc("/int/admin/sf/attachment/", baseHandler("/int/admin/cvsync/sf/attachment/", getAttachment))
	http.HandleFunc("/int/admin/sf/attachment-text/", baseHandler("/int/admin/cvsync/sf/attachment-text/", getAttachmentText))
	http.HandleFunc("/int/admin/sf/attachment-extraction/", baseHandler("/int/admin/cvsync/sf/attachment-extraction/", getAttachmentExtraction))
	http.HandleFunc("/int/admin/sf/open-leads/", baseHandler("/int/admin/sf/open-leads", handleOpenLeads))
	http.HandleFunc("/int/admin/sf/", baseHandler("/int/admin/sf/", handleProxySalesForce))

	http.HandleFunc("/int/srv/Batch/Add", baseHandler("/int/srv/Batch/Add", handleBatchAdd))
	http.HandleFunc("/int/srv/Batch/Ingest/Status/", baseHandler("/int/srv/Batch/Status/", handleBatchIngestStatus))
	http.HandleFunc("/int/srv/Batch/Query/Status/", baseHandler("/int/srv/Query/Status/", handleBatchQueryStatus))
	http.HandleFunc("/int/srv/Batch/Query/Data/", baseHandler("/int/srv/Batch/Query/Data/", handleBatchQueryData))

	http.HandleFunc("/int/srv/Batch/SelectWithResponse", baseHandler("/int/srv/Batch/SelectWithResponse", handleBatchSelectWithResponse))
	http.HandleFunc("/int/srv/sf/Status", baseHandler("/int/srv/Status", handleStatus))
	http.HandleFunc("/int/srv/geoip/", baseHandler("/int/srv/geoip", handleGeoIP))
	http.HandleFunc("/int/srv/geosearch/", baseHandler("/int/srv/geosearch", handleGeoSearch))
	http.HandleFunc("/int/srv/JobApplication/StatusCounts", baseHandler("/int/srv/JobApplication/StatusCounts", jobAppStatusCountsPerUser))
	http.HandleFunc("/int/srv/JobApplication/StatusCounts/", baseHandler("/int/srv/JobApplication/StatusCounts/", jobAppStatusCountsPerUser))
	http.HandleFunc("/int/srv/JobApplication/StatusCountsDetails/", baseHandler("/int/srv/JobApplication/StatusCountsDetails/", jobAppStatusCountsPerUserDetails))
	http.HandleFunc("/int/srv/JobApplication/EmailStatus/", baseHandler("/int/srv/JobApplication/EmailStatus/", handleUpdateJobApplicationEmailStatus))
	http.HandleFunc("/int/srv/Package/Revenue", baseHandler("/int/srv/Package/Revenue/", soldPackageRevenuePerUser))
	http.HandleFunc("/int/srv/Package/Revenue/", baseHandler("/int/srv/Package/Revenue/", soldPackageRevenuePerUser))
	http.HandleFunc("/int/srv/Package/RevenueDetails/", baseHandler("/int/srv/Package/RevenueDetails/", soldPackageRevenuePerUserDetails))
	http.HandleFunc("/int/srv/Package/RevenueSlides/", baseHandler("/int/srv/Package/RevenueSlides/", soldPackageSlideDataSince))
	http.HandleFunc("/int/srv/Package/Corrections/", baseHandler("/int/srv/Package/Corrections/", HandlePackageCorrections))
	http.HandleFunc("/int/srv/Package/MonthlyPayments", baseHandler("/int/srv/Package/MonthlyPayments", RetrievePackageAllMonthlyPayments))
	http.HandleFunc("/int/srv/Users", baseHandler("/int/srv/Users", handleUsers))
	http.HandleFunc("/int/srv/UserPhoto/", baseHandler("/int/srv/UserPhoto/", handleUserPhoto))
	http.HandleFunc("/int/srv/Query", baseHandler("/int/srv/Query", handleQuery))
	http.HandleFunc("/int/srv/Picklist/Countries/", baseHandler("/int/srv/Picklist/Countries/", handleCountriesList))
	http.HandleFunc("/int/srv/c", baseHandler("/int/srv/c", func(w http.ResponseWriter, r *http.Request) {
		c, err := entities.RetrieveCandidatesForSolrSinceLastModified(nil)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		JSONOutput(w, map[string]interface{}{"Candidates": c})
	}))
	http.HandleFunc("/int/srv/", baseHandler("/int/srv/", handleProxySalesForce))

	//http.HandleFunc("/int/srv/LeadInfoPush", baseHandler("/int/srv/LeadInfoPush", handleLeadInfoPush))

	http.HandleFunc("/Cities", baseHandler("/Cities", handleCities))
	http.HandleFunc("/Geocode/", baseHandler("/Geocode/", handleGeocode))
	http.HandleFunc("/SiteDomain", baseHandler("/SiteDomain", handleSiteDomain))

	// Not really used, or helper urls for development
	http.HandleFunc("/picklists/", baseHandler("/picklists/", handlePicklists))
	http.HandleFunc("/fields/", baseHandler("/fields/", handleFields))
	http.HandleFunc("/Job/ID/", baseHandler("/Job/ID/", handleJobsID))
	http.HandleFunc("/AutoComplete/City/", baseHandler("/AutoComplete/City/", handleAutocompleteCity))
	http.HandleFunc("/Provinces", baseHandler("/Provinces", handleProvinces))
	http.HandleFunc("/Candidate/EmailCounts", baseHandler("/Candidate/EmailCounts", handleCandidateEmailCounts))
	http.HandleFunc("/HashPassword/", baseHandler("/HashPassword/", handleHashPassword))
	http.HandleFunc("/hardcriteriaspecialisms", baseHandler("/hardcriteriaspecialisms", handleHCSpecialisms))
	http.HandleFunc("/Raw/Accounts", baseHandler("/Raw/Accounts", handleAccountsFull))
	http.HandleFunc("/SaveJobHashTest/", baseHandler("/SaveJobHashTest/", saveJobHash))

	// http.HandleFunc("/TestStream", baseHandler("/TestStream", func(w http.ResponseWriter, r *http.Request) {
	// 	log.Println("test stream method")
	// 	prevSet := switchsettings.Settings.JobStreamDisabled
	// 	switchsettings.Settings.JobStreamDisabled = false
	// 	msg := salesforcestream.StreamMSG{}
	// 	msg.Data.Payload.ChangeEventHeader.EntityName = "cxsrec__cxsPosition__c"
	// 	msg.Data.Payload.ChangeEventHeader.ChangeType = "UPDATE"
	// 	msg.Data.Payload.ChangeEventHeader.RecordIDs = []string{"a0w2p00000C8h6SAAR"}
	// 	msg.Data.Payload.ChangeEventHeader.CommitTimestamp = uint64(time.Now().UnixMilli())
	// 	entities.HandleChangeStream(msg)
	// 	switchsettings.Settings.JobStreamDisabled = prevSet
	// 	w.Write([]byte("Done"))
	// }))

	http.HandleFunc("/", baseHandler("/", handleProxySalesForce))
}

//	func handleTestCreateHC(w http.ResponseWriter, r *http.Request) {
//		data := map[string]interface{}{
//			"RecordTypeId":                    "0122p000000R7glAAC",
//			"cxsrec__Hard_Criterium_Value__c": "a0Z2p00000a6jQBEAY",
//			"cxsrec__Position__c":             "a0w2p000007uphWAAQ",
//		}
//		body, err := salesforcerest.RequestToEndpoint(http.MethodPost, "sobjects/cxsrec__cxsHard_criterium__c", data)
//		w.Write(body)
//		w.Write([]byte(err.Error()))
//	}

func makeFuzzy(searchStruct solr.Search) solr.Search {
	strps := strings.Split(searchStruct.SearchString, " ")
	for k, v := range strps {
		if len(v) > 3 {
			strps[k] += fmt.Sprintf("~%v", math.Round(float64(len(v))/3))
		}
	}
	searchStruct.SearchString = strings.Join(strps, " ")
	return searchStruct
}
func handleQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	body, err := salesforcerest.SelectRequest(query, -1)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, body)
}
func handleBatchAdd(w http.ResponseWriter, r *http.Request) {
	var batchRequestInput struct {
		Operation string
		SObject   string
		Data      interface{}
		Query     string
	}
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("only POST allowed"), http.StatusBadRequest, true, false)
		return
	}
	err := json.NewDecoder(r.Body).Decode(&batchRequestInput)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	jobID := ""
	nrofrecords := 0
	if batchRequestInput.Operation == "query" {
		if strings.TrimSpace(batchRequestInput.Query) == "" {
			JSONError(w, r, fmt.Errorf("no query"), http.StatusBadRequest, true, false)
			return
		}
		jobID, err = salesforcerest.BatchRequestQuery(batchRequestInput.Query, -1)
		if err != nil {
			JSONError(w, r, fmt.Errorf("handleBatchAdd BatchRequestQuery %v Query: %s", err, batchRequestInput.Query), http.StatusInternalServerError, true, false)
			return
		}
	} else {
		jobID, nrofrecords, err = salesforcerest.BatchRequestIngest(batchRequestInput.Operation, batchRequestInput.SObject, batchRequestInput.Data, -1)
		if err != nil {
			JSONError(w, r, fmt.Errorf("handleBatchAdd BatchRequestIngest %v", err), http.StatusInternalServerError, true, false)
			return
		}
	}
	JSONOutput(w, struct {
		JobID                  string `json:"jobId"`
		NumberRecordsProcessed int    `json:"numberRecordsProcessed"`
	}{jobID, nrofrecords})
}
func handleBatchIngestStatus(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Status", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest, true, false)
		return
	}
	result, err := salesforcerest.BatchRequestIngestStatus(fragments[0], -1)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, result)
}
func handleBatchQueryStatus(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Status", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest, true, false)
		return
	}
	result, err := salesforcerest.BatchRequestQueryStatus(fragments[0], -1)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, result)
}
func handleBatchQueryData(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Data", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest, true, false)
		return
	}
	result, err := salesforcerest.BatchRequestQueryData(fragments[0], -1)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, result)
}
func handleBatchSelectWithResponse(w http.ResponseWriter, r *http.Request) {
	query := r.FormValue("query")
	if strings.TrimSpace(query) == "" {
		JSONError(w, r, fmt.Errorf("param 'query' cannot be empty"), http.StatusBadRequest, true, false)
	}
	result, err := salesforcerest.BatchRequestQueryWithResponse(query, -1)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if r.FormValue("idkeyed") == "true" {
		response := map[string]*map[string]string{}
		for _, v := range result {
			response[v["Id"]] = &v
		}
		JSONOutput(w, response)
		return
	}

	JSONOutput(w, result)
}

// handleOpenLeads display all the open leads, see entities.RetrieveOpenLeads for what is cached
func handleOpenLeads(w http.ResponseWriter, r *http.Request) {
	leads := entities.CachedOpenLeads.GetAll()
	_, _ = w.Write([]byte(helpers.PrettyPrintReturn(leads)))
}

func handleCountriesList(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Countries", r.URL.Path)
	list, err := sfids.GetCountriesForLanguage(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	JSONOutput(w, list)
}
func candidateIDsSync(w http.ResponseWriter, r *http.Request) {
	err := entities.SyncUpdateCandidatesIDs()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, map[string]bool{"success": true})
}
func cvSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if err := tpls.Render(w, "cvsync", struct {
		PageCVList    string
		PageVirusList string
	}{
		r.URL.Query().Get("pagecvlist"),
		r.URL.Query().Get("pageviruslist"),
	}); err != nil {
		log.Println(err)
	}
}
func cvSyncData(w http.ResponseWriter, r *http.Request) {
	CandidateCount, err := entities.GetCandidatesCountDatabase()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}

	CVsPerPage := 20
	pageCVList, _ := strconv.Atoi(r.URL.Query().Get("pagecvlist"))
	if pageCVList < 1 {
		pageCVList = 1
	}
	CVs, countCV, err := entities.GetCVAttachmentsDatabase(pageCVList, CVsPerPage)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	CVItems := ""
	for _, i := range CVs {
		CVItems += fmt.Sprintf(`<div class='row'><div class='cell'>%s</div><div class='cell'>%s</div><div class='cell'><a target='_blank' href='attachment-extraction/%v'>%s</a></div></div>`, i.UploadedAt.Format("2006-01-02"), i.TextExtractType, i.Id, i.OrigFilename)
	}
	CVPagination := Pagination(uint16(math.Ceil(float64(countCV)/float64(CVsPerPage))), uint16(pageCVList), `javascript: setPage("pagecvlist", %v)`)

	VirussesPerPage := 20
	pageVirusList, _ := strconv.Atoi(r.URL.Query().Get("pageviruslist"))
	if pageVirusList < 1 {
		pageVirusList = 1
	}
	Virusses, countVirus, err := entities.GetVirusInfectedFilesList(pageVirusList, VirussesPerPage)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	VirusItems := ""
	for _, i := range Virusses {
		VirusItems += fmt.Sprintf(`<div class='row'><div class='cell'>%s</div><div class='cell'>%s</div></div>`, i.VirusScannedAt.Format("2006-01-02"), i.VirusName)
	}
	VirusPagination := Pagination(uint16(math.Ceil(float64(countVirus)/float64(VirussesPerPage))), uint16(pageVirusList), `javascript: setPage("pageviruslist", %v)`)
	JSONOutput(w, struct {
		CandidateCount  int
		CVCount         int
		CVItems         template.HTML
		CVPagination    template.HTML
		VirusCount      int
		VirusItems      template.HTML
		VirusPagination template.HTML
	}{
		CandidateCount:  CandidateCount,
		CVCount:         countCV,
		CVItems:         template.HTML(CVItems),
		CVPagination:    CVPagination,
		VirusCount:      countVirus,
		VirusItems:      template.HTML(VirusItems),
		VirusPagination: VirusPagination,
	})

}
func getAttachment(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("attachment", r.URL.Path)
	if len(fragments) != 1 {
		HTMLError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest)
		return
	}
	// id, err := strconv.ParseInt(fragments[0], 10, 64)
	// if err != nil {
	// 	HTMLError(w, r, err, http.StatusBadRequest)
	// }
	// attachment, f, err := entities.GetAttachmentDatabaseBinary(id)
	// if err != nil {
	// 	HTMLError(w, r, err, http.StatusNotFound)
	// 	return
	// }
	// w.Header().Set("Content-Type", attachment.MimeType) //"application/octet-stream"
	// w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, attachment.OrigFilename))
	// w.Header().Set("Content-Length", fmt.Sprintf("%v", attachment.ContentLength))
	w.Header().Set("Cache-Control", "private")
	w.Header().Set("Pragma", "private")
	w.Header().Set("Expires", "Mon, 26 Jul 1997 05:00:00 GMT")

	w.WriteHeader(http.StatusOK)

	// _, err = io.Copy(w, f)
	// if err != nil {
	// 	HTMLError(w, r, err, http.StatusInternalServerError)
	// 	return
	// }
}
func getAttachmentText(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("attachment-text", r.URL.Path)
	if len(fragments) != 1 {
		HTMLError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(fragments[0], 10, 64)
	if err != nil {
		HTMLError(w, r, err, http.StatusBadRequest)
	}
	attachment, err := entities.GetAttachmentDatabase(id)
	if err != nil {
		HTMLError(w, r, err, http.StatusNotFound)
		return
	}
	if _, err = w.Write([]byte(attachment.TextExtractTxt)); err != nil {
		HTMLError(w, r, err, http.StatusInternalServerError)
		return
	}
}
func getAttachmentExtraction(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("attachment-extraction", r.URL.Path)
	if len(fragments) != 1 {
		HTMLError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest)
		return
	}
	html := `<!DOCTYPE html>
	<html>
	<head>
		<meta charset="utf-8">
		<style>
		body, html {
			margin: 0;
			padding: 0;
			height: 100%%;
			width: 100%%;
		}
		body {
			display:flex;
		}
		iframe {
			margin: 0;
			padding: 0;
			border: 0;
			width: 50%%;
			height: 100%%;
		}
		</style>
	</head>
	<body>
	<iframe frameBorder="0" id="left_frame" src="../attachment-text/%s"></iframe>
	<iframe frameBorder="0" id="right_frame" src="../attachment/%s"></iframe>
	</body>
	</html>`
	w.Header().Set("Content-Type", "text/html")
	if _, err := w.Write([]byte(fmt.Sprintf(html, fragments[0], fragments[0]))); err != nil {
		HTMLError(w, r, fmt.Errorf("error"), http.StatusInternalServerError)
		return
	}
}
func processBatchOfCandidatesForCVSync(w http.ResponseWriter, r *http.Request) {
	if err := entities.ProcessBatchOfCandidatesForCVSync(); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, map[string]bool{"success": true})
}
func isRunningBatchOfCandidatesForCVSync(w http.ResponseWriter, _ *http.Request) {
	JSONOutput(w, map[string]bool{"running": entities.CVSyncRunning})
}
func stopAndClearCVSync(w http.ResponseWriter, r *http.Request) {
	err := entities.StopAndClearCVSync()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	JSONOutput(w, map[string]bool{"success": true})
}
func handleUserPhoto(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("UserPhoto", r.URL.Path)
	if len(fragments) != 1 {
		HTMLError(w, r, fmt.Errorf("bad request"), http.StatusBadRequest)
		return
	}
	bytes, err := entities.GetUserPhoto(fragments[0])
	if err != nil {
		HTMLError(w, r, err, http.StatusNotFound)
		return
	}
	w.Header().Add("Content-Type", http.DetectContentType(bytes))
	w.Header().Add("Content-Length", fmt.Sprintf("%v", len(bytes)))
	if _, err = w.Write(bytes); err != nil {
		JSONError(w, r, fmt.Errorf("could not write to output"), http.StatusInternalServerError, true, false)
		return
	}
}
func handleUpdateJobApplicationEmailStatus(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("EmailStatus", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("need id and mailtype in url"), http.StatusBadRequest, true, false)
		return
	}
	jobAppID := fragments[0]
	mailType := fragments[1]
	if !(mailType == string(entities.CANDIDATEMAIL) || mailType == string(entities.COMPANYMAIL)) {
		JSONError(w, r, fmt.Errorf("maitype needs to be '%s' or '%s'", entities.CANDIDATEMAIL, entities.COMPANYMAIL), http.StatusBadRequest, true, false)
		return
	}
	var input struct {
		Status     string    `json:"status"`
		StatusDate time.Time `json:"statusdate"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}

	//log.Println(input.StatusDate, input.StatusDate.Local())

	if err := entities.UpdateJobApplicationEmailStatus(jobAppID, entities.Mailtype(mailType), input.Status, input.StatusDate); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(map[string]bool{"success": true}); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobData(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("JobData", r.URL.Path)
	jobID := fragments[0]

	jobs := entities.CachedJobs.GetAll(true)
	job, ok := jobs[jobID]

	if !ok {
		var err error
		job, err = entities.RetreivePosition(jobID)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		companies, err := entities.RetrieveAccounts([]string{job.Full.Account})
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		company, ok := companies[job.Full.Account]
		if !ok {
			if err != nil {
				JSONError(w, r, fmt.Errorf("cannot find company belonging to job"), http.StatusInternalServerError, true, false)
				return
			}
		}
		_, _, jobLabelsByGroupKeyAndValueComp, _ := entities.CachedCompanies.GetLabelgroups()
		company4net, err := entities.AccountToCompany2XML(company, jobLabelsByGroupKeyAndValueComp)
		if err != nil {
			JSONError(w, r, fmt.Errorf("cannot convert company data"), http.StatusInternalServerError, true, false)
			return
		}
		recruiters := entities.CachedContacts.GetRecruiters()
		_, _, jobLabelsByGroupKeyAndIDJob, jobLabelsByGroupKeyAndValueJob, _ := entities.CachedJobs.GetPositionsAndLabelgroups()
		err = job.To4net(recruiters, jobLabelsByGroupKeyAndIDJob, jobLabelsByGroupKeyAndValueJob, company4net)
		if err != nil {
			JSONError(w, r, fmt.Errorf("cannot convert job data"), http.StatusInternalServerError, true, false)
			return
		}
	}

	solrJob, err := entities.ConvertJobXMLToSolr(job.As4net)
	if err != nil {
		JSONError(w, r, fmt.Errorf("cannot convert job data to solr type"), http.StatusInternalServerError, true, false)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(map[string]*entities.SolrDocumentJob{"job": solrJob}); err != nil {
		JSONError(w, r, fmt.Errorf("cannot encode job"), http.StatusInternalServerError, true, false)
		return
	}
}
func handleAllAddresses(w http.ResponseWriter, r *http.Request) {
	accountAddresses, err := entities.RetrieveAccountAddresses()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	positionAddresses, err := entities.RetrievePositionAddresses()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(map[string][]string{"Accounts": accountAddresses, "Jobs": positionAddresses}); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fragments := helpers.GetFragmentsAfter("Counts", r.URL.Path)
	if len(fragments) == 0 {
		fragments = append(fragments, "")
	}
	SOBJ := map[string]entities.Sobject{
		"Positions":       entities.SOBJ_Position,
		"JobApplications": entities.SOBJ_JobApplication,
		"Accounts":        entities.SOBJ_Account,
		"Candidates":      entities.SOBJ_Candidate,
		"SoldPackages":    entities.SOBJ_SoldPackage,
	}[fragments[0]]
	data := struct {
		Type          entities.Sobject
		Data          []*entities.StepEventCounts
		ImportantKeys map[string]bool
	}{
		Type: SOBJ,
		Data: []*entities.StepEventCounts{},
	}
	var err error
	if SOBJ != "" {
		data.Data, err = entities.CachedSteps.Aggregate(SOBJ)
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}
	}
	err = tpls.Render(w, "counts", data)
	if err != nil {
		log.Println(err)
	}
}

func handleLeadVisits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fragments := helpers.GetFragmentsAfter("LeadVisits", r.URL.Path)
	baseURL := strings.Split(r.RequestURI, "LeadVisits")[0] + "LeadVisits"
	if len(fragments) > 0 {
		if !(fragments[0] == "leadfeeder" || fragments[0] == "leadinfo" || fragments[0] == "leadcsv") {
			fragments = []string{}
		}
	}
	if len(fragments) == 0 {
		w.Write([]byte("<center><br><br><a href='./leadfeeder'>leadfeeder</a> | <a href='./leadinfo'>leadinfo</a></center>"))
		return
	}

	var visittype = fragments[0]
	if visittype == "leadinfo" && len(fragments) > 1 {
		w.Write([]byte(fmt.Sprintf("<center><br><br>Cannot specify dates with leadinfo, click here to get the last 48 hours: <a href='%s/leadinfo'>leadinfo</a></center>", baseURL)))
		return
	}
	var ts, te time.Time
	now := time.Now()
	var err error
	if len(fragments) == 1 {
		ts = now
		te = now
	} else if len(fragments) == 2 {
		ts, err = time.Parse("2006-01-02", fragments[1])
		if err == nil {
			te = ts
		}
	} else if len(fragments) == 3 {
		ts, err = time.Parse("2006-01-02", fragments[1])
		if err == nil {
			te, err = time.Parse("2006-01-02", fragments[2])
		}
	} else {
		err = fmt.Errorf("too many arguments")
	}

	if err != nil {
		JSONError(w, r, fmt.Errorf("we need ../{optional:startdate}/{optional:enddate}  (error: %v)", err), http.StatusBadRequest, true, false)
		return
	}
	min := .4
	max := .9
	if vals, ok := r.URL.Query()["min"]; ok && len(vals) > 0 {
		if m, err := strconv.ParseFloat(vals[0], 64); err == nil {
			min = m
		}
	}
	if vals, ok := r.URL.Query()["max"]; ok && len(vals) > 0 {
		if m, err := strconv.ParseFloat(vals[0], 64); err == nil {
			max = m
		}
	}
	var visits []leadvisits.Visit
	if visittype == "leadfeeder" {
		visits, err = leadvisits.GetLeadfeederVisits(ts, te)
	} else if visittype == "leadinfo" {
		visits, err = leadvisits.GetLeadinfoVisits()
	} else if visittype == "leadcsv" {
		visits, err = leadvisits.GetLeadcsvVisits()
	}
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	parseResponse, err := leadvisits.ParseVisits(visits, min, max)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	canCommitToSF := visittype == "leadinfo" || now.Format("2006-01-02") == te.Format("2006-01-02")
	if vals, ok := r.URL.Query()["commit"]; ok && len(vals) > 0 {
		if vals[0] == "true" {
			if !canCommitToSF {
				JSONError(w, r, fmt.Errorf("the end date is not today, comitting to Salesforce is blocked"), http.StatusBadRequest, true, false)
				return
			}
			actions := leadvisits.CommitParsedVisitResults(parseResponse)
			w.Write([]byte(strings.Join(actions, "<br/>")))
			return
		}
	}
	err = tpls.Render(w, "leadfeeder", map[string]interface{}{"baseURL": baseURL, "visits": parseResponse.Visits, "accountleads": parseResponse.LeadsPerAccount, "accountopps": parseResponse.OppsPerAccount, "leadopps": parseResponse.OppsPerLead, "donothing": leadvisits.DO_NOTHING, "showactionsbtn": canCommitToSF})
	if err != nil {
		log.Println(err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

//	func handleLeadInfoPush(w http.ResponseWriter, r *http.Request) {
//		var post leadvisits.LeadInfoLeadVisit
//		if r.Method == http.MethodPost {
//			err := json.NewDecoder(r.Body).Decode(&post)
//			if err != nil {
//				http.Error(w, err.Error(), http.StatusBadRequest)
//				return
//			}
//		}
//		err := leadvisits.AddLeadinfoVisits(post)
//		if err != nil {
//			http.Error(w, err.Error(), http.StatusInternalServerError)
//			return
//		}
//		w.Write([]byte("success"))
//	}
func handleStreamMessages(w http.ResponseWriter, r *http.Request) {
	messages, err := salesforcestream.DebugStreamMessages()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(messages); err != nil {
		log.Println("handleStreamMessages Encode JSON failed", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleStreamEventHistory(w http.ResponseWriter, r *http.Request) {

	file, err := os.Open(filepath.Join(cfg.LogDir, "streamevents.log"))
	if err != nil {
		HTMLError(w, r, err, http.StatusInternalServerError)
		return
	}
	defer file.Close()

	//cutoffDate := time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02")
	scanner := bufio.NewScanner(file)
	eventsGrouped := map[string]map[string]map[string]int{}
	for scanner.Scan() {
		eventparts := strings.Split(scanner.Text(), " ")
		date := eventparts[0]
		// if date < cutoffDate {
		// 	continue
		// }
		action := eventparts[4]
		itemtype := eventparts[5]
		if _, ok := eventsGrouped[date]; !ok {
			eventsGrouped[date] = map[string]map[string]int{}
		}
		if _, ok := eventsGrouped[date][action]; !ok {
			eventsGrouped[date][action] = map[string]int{}
		}
		if _, ok := eventsGrouped[date][action][itemtype]; !ok {
			eventsGrouped[date][action][itemtype] = 0
		}
		eventsGrouped[date][action][itemtype]++
	}
	eventsSlice := []struct {
		Date     string
		Action   string
		ItemType string
		Count    int
	}{}
	for date, actions := range eventsGrouped {
		for action, itemtypes := range actions {
			for itemtype, count := range itemtypes {
				eventsSlice = append(eventsSlice, struct {
					Date     string
					Action   string
					ItemType string
					Count    int
				}{
					Date:     date,
					Action:   action,
					ItemType: itemtype,
					Count:    count,
				})
			}
		}
	}
	sort.Slice(eventsSlice, func(i, j int) bool {
		if eventsSlice[i].Date != eventsSlice[j].Date {
			return eventsSlice[i].Date > eventsSlice[j].Date
		}
		if eventsSlice[i].Action != eventsSlice[j].Action {
			return eventsSlice[i].Action < eventsSlice[j].Action
		}
		return eventsSlice[i].ItemType < eventsSlice[j].ItemType
	})
	totals := map[string]int{}
	html := "<html><head></head><body><style>* { font-family:verdana; font-size:11px;}</style><table><tr><th>Date</th><th>Action</th><th>ItemType</th><th>Count</th></tr>"
	curdate := eventsSlice[0].Date
	dateTotal := 0
	cutOffDate := time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02")
	for _, event := range eventsSlice {
		if event.Date != curdate {
			if event.Date >= cutOffDate {
				html += "<tr><td colspan=3>Total</td><td>" + strconv.Itoa(dateTotal) + "</td></tr><tr><td colspan=4>&nbsp;</td></tr>"
			}
			totals[curdate] = dateTotal
			curdate = event.Date
			dateTotal = 0
		}
		dateTotal += event.Count
		if event.Date > cutOffDate {
			html += fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td></tr>", event.Date, event.Action, event.ItemType, event.Count)
		}
	}
	//html += "<tr><td colspan=3>Total</td><td>" + strconv.Itoa(dateTotal) + "</td></tr><tr><td colspan=4>&nbsp;</td></tr>"
	html += "</table>"
	html += "<div>This graph is from since " + eventsSlice[len(eventsSlice)-1].Date + "</div>"
	html += "<div style='display:flex;align-items: baseline;'>"
	for _, total := range totals {
		html += fmt.Sprintf("<div style='height:%dpx;width:2px;background-color:#000'></div>", total/100)
	}
	html += "</div>"
	html += "</body></html>"
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

// jobAppStatusCountsPerUser is used by Targetsheet
func jobAppStatusCountsPerUser(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("StatusCounts", r.URL.Path)
	if len(fragments) == 0 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	userType := fragments[0]
	now := time.Now()
	year := now.Format("2006")
	monthnr := now.Format("1")
	monthcount := 1
	if len(fragments) > 1 && strings.TrimSpace(fragments[1]) != "" {
		year = fragments[1]
	}
	if len(fragments) > 2 && strings.TrimSpace(fragments[2]) != "" {
		monthnr = fragments[2]
	}
	if len(fragments) > 3 && strings.TrimSpace(fragments[3]) != "" {
		monthcount, _ = strconv.Atoi(fragments[3])
	}

	userIDs := []string{}
	limitUsers := false
	if r.Method == http.MethodPost {
		err := json.NewDecoder(r.Body).Decode(&userIDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limitUsers = true
	}

	result, err := entities.RetrieveStatusCountsPerUser(userType, year, monthnr, monthcount, limitUsers, userIDs)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("jobAppStatusCountsPerUser Encode JSON failed", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

// RetrievePackageAllMonthlyPayments used by Targetsheet
func RetrievePackageAllMonthlyPayments(w http.ResponseWriter, r *http.Request) {
	result, err := entities.RetrievePackageAllMonthlyPayments()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("RetrievePackageAllMonthlyPayments Encode JSON failed", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

// jobAppStatusCountsPerUserDetails used by Targetsheet
func jobAppStatusCountsPerUserDetails(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("StatusCountsDetails", r.URL.Path)
	if len(fragments) == 0 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	userType := fragments[0]
	userID := fragments[1]
	now := time.Now()
	year := now.Format("2006")
	monthnr := now.Format("1")
	monthcount := 1
	if len(fragments) > 2 && strings.TrimSpace(fragments[2]) != "" {
		year = fragments[2]
	}
	if len(fragments) > 3 && strings.TrimSpace(fragments[3]) != "" {
		monthnr = fragments[3]
	}
	if len(fragments) > 4 && strings.TrimSpace(fragments[4]) != "" {
		monthcount, _ = strconv.Atoi(fragments[4])
	}

	result, err := entities.RetrieveStatusCountsPerUserDetails(userType, userID, year, monthnr, monthcount)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("jobAppStatusCountsPerUserDetails could not Encode JSON", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

// handleUsers used by Targetsheet
func handleUsers(w http.ResponseWriter, r *http.Request) {
	userIDs := []string{}
	limitUsers := false
	if r.Method == http.MethodPost {
		err := json.NewDecoder(r.Body).Decode(&userIDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limitUsers = true
	}
	users, err := entities.RetrieveUsers(limitUsers, userIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(users); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// soldPackageSlideDataSince used by Targetsheet
func soldPackageSlideDataSince(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("RevenueSlides", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	startdate, err := time.Parse("2006-1-2T15:04:05", fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	var result = struct {
		NextQueryTime string
		Packages      []*entities.PackageRevenueSlideFlat
	}{
		NextQueryTime: fragments[0],
		Packages:      []*entities.PackageRevenueSlideFlat{},
	}
	data, toTime, err := entities.RetrieveTargetSheetSlideDataSince(startdate)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	result.NextQueryTime = toTime.Format("2006-01-02T15:04:05")
	result.Packages = data
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("soldPackageSlideDataSince Encode JSON failed", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func HandlePackageCorrections(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Corrections", r.URL.Path)
	now := time.Now()
	year := now.Format("2006")
	monthnr := now.Format("1")
	monthcount := 1
	if len(fragments) > 0 && strings.TrimSpace(fragments[0]) != "" {
		year = fragments[0]
	}
	if len(fragments) > 1 && strings.TrimSpace(fragments[1]) != "" {
		monthnr = fragments[1]
	}
	if len(fragments) > 2 && strings.TrimSpace(fragments[2]) != "" {
		monthcount, _ = strconv.Atoi(fragments[2])
	}
	result, err := entities.RetrievePackageCorrections(year, monthnr, monthcount)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(result); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

// soldPackageRevenuePerUserDetails used by Targetsheet
func soldPackageRevenuePerUserDetails(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("RevenueDetails", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	userType := fragments[0]
	userID := fragments[1]

	result := struct {
		SoldPackages      []*entities.PackageRevenueDetailsFlat
		PlacementPackages []*entities.PackageRevenueDetailsFlat
		Corrections       []*entities.PackageRevenueDetailsFlat
	}{
		SoldPackages:      []*entities.PackageRevenueDetailsFlat{},
		PlacementPackages: []*entities.PackageRevenueDetailsFlat{},
		Corrections:       []*entities.PackageRevenueDetailsFlat{},
	}
	var err error
	if userType == "salesconsultant" {
		result.SoldPackages, err = entities.RetrieveRevenueSoldPackagesSalesConsultantDetails(userID)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		result.PlacementPackages, err = entities.RetrieveRevenuePlacementPackagesDetails(userType, userID)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		result.Corrections, err = entities.RetrievePackageCorrectionsDetails(userID)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	} else if userType == "resourcer" {
		result.PlacementPackages, err = entities.RetrieveRevenuePlacementPackagesDetails(userType, userID)
	} else {
		JSONError(w, r, fmt.Errorf("user type needs to be 'resourcer' or 'salesconsultant'"), http.StatusInternalServerError, true, false)
		return
	}
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("soldPackageRevenuePerUserDetails Encode JSON failed", err)
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

// soldPackageRevenuePerUser used by Targetsheet
func soldPackageRevenuePerUser(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Revenue", r.URL.Path)
	if len(fragments) == 0 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	userType := fragments[0]
	now := time.Now()
	year := now.Format("2006")
	monthnr := now.Format("1")
	monthcount := 1
	if len(fragments) > 1 && strings.TrimSpace(fragments[1]) != "" {
		year = fragments[1]
	}
	if len(fragments) > 2 && strings.TrimSpace(fragments[2]) != "" {
		monthnr = fragments[2]
	}
	if len(fragments) > 3 && strings.TrimSpace(fragments[3]) != "" {
		monthcount, _ = strconv.Atoi(fragments[3])
	}

	userIDs := []string{}
	limitUsers := false
	// if r.Method == http.MethodPost {
	// 	err := json.NewDecoder(r.Body).Decode(&userIDs)
	// 	if err != nil {
	// 		http.Error(w, err.Error(), http.StatusBadRequest)
	// 		return
	// 	}
	// 	limitUsers = true
	// }

	var result []*entities.PackageRevenue
	var err error
	if userType == "salesconsultant" {
		result, err = entities.RetrieveRevenueSoldPackagesSalesConsultant(year, monthnr, monthcount, limitUsers, userIDs)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		result2, err := entities.RetrieveRevenuePlacementPackages(userType, year, monthnr, monthcount, limitUsers, userIDs)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		result = append(result, result2...)
		result3, err := entities.RetrievePackageCorrections(year, monthnr, monthcount)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		result = append(result, result3...)
	} else if userType == "resourcer" {
		result, err = entities.RetrieveRevenuePlacementPackages(userType, year, monthnr, monthcount, limitUsers, userIDs)
	} else {
		JSONError(w, r, fmt.Errorf("user type needs to be 'resourcer' or 'salesconsultant'"), http.StatusInternalServerError, true, false)
		return
	}
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Println("soldPackageRevenuePerUser encode JSON failed", err)
	}
}
func jobsSearchHandler(w http.ResponseWriter, r *http.Request) {
	_, _, _, _, labelsByGroupKeyAndPerma := entities.CachedJobs.GetPositionsAndLabelgroups()

	searchStruct, loc, err := createSolrSearchStruct("Jobs", r, labelsByGroupKeyAndPerma, nil)
	if err != nil {
		err = fmt.Errorf("(From %s, %s): %s\nOrig URL: %s", r.Header.Get("X-FORWARDED-FOR"), r.Header.Get("REFERER"), err, r.Header.Get("URL"))
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if searchStruct.Map == 1 {
		searchStruct.Location = ""
		searchStruct.LatLng = ""
	}
	var results solr.SolrQueryCallResultJobs
	if err := solrCoreJobs.Search(searchStruct, &results); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if results.Response.NumFound == 0 {
		searchStruct = makeFuzzy(searchStruct)
		if err := solrCoreJobs.Search(searchStruct, &results); err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
	response := entities.JobSearchReturn{
		HasMore:         results.Response.NumFound > results.Response.Start+len(results.Response.Docs),
		NumberOfPages:   int(math.Ceil(float64(results.Response.NumFound) / float64(searchStruct.Rows))),
		RowCount:        results.Response.NumFound,
		CategoryAmounts: map[string]int{},
		Location:        loc,
	}
	if searchStruct.Map == 1 {
		jobsMapResult := []*entities.SolrDocumentJobMap{}
		for _, v := range results.Response.Docs {
			jobsMapResult = append(jobsMapResult, &entities.SolrDocumentJobMap{
				Id:     v.Id,
				LatLng: v.LatLng,
			})
		}
		response.JobsMap = jobsMapResult
	} else {
		response.Jobs = results.Response.Docs
	}
	facetFieldToLabelgroupKey := map[string]string{}
	titleTrans := cases.Title(language.Dutch)
	for labelgroupkey := range labelsByGroupKeyAndPerma {
		camelCase := strings.ReplaceAll(titleTrans.String(strings.ReplaceAll(strings.ToLower(labelgroupkey), "_", " ")), " ", "")
		facetFieldToLabelgroupKey[camelCase] = labelgroupkey
	}
	//helpers.PrettyPrint(facetFieldToLabelgroupKey)
	facetFieldToLabelgroupKey["Countries"] = "COUNTRY"
	facetFieldToLabelgroupKey["LanguageSkills"] = "LANGUAGE"

	pivots := results.FacetCounts.FacetPivots["FunctionLevel1,FunctionLevel2,FunctionLevel3"]

	labelPerFullPerma := entities.CachedJobs.GetFullPermaToLabel()
	for _, pivot1 := range pivots {
		uri1 := pivot1.Value
		if lbl1, ok := labelPerFullPerma[uri1]; ok {
			response.CategoryAmounts[lbl1.ID] = int(pivot1.Count)
			for _, pivot2 := range pivot1.Pivot {
				uri2 := uri1 + "/" + pivot2.Value
				if lbl2, ok := labelPerFullPerma[uri2]; ok {
					response.CategoryAmounts[lbl2.ID] = int(pivot2.Count)
					for _, pivot3 := range pivot2.Pivot {
						uri3 := uri2 + "/" + pivot3.Value
						if lbl3, ok := labelPerFullPerma[uri3]; ok {
							response.CategoryAmounts[lbl3.ID] = int(pivot3.Count)
						} else {
							log.Println("Cannot find label for uri: ", uri3)
						}
					}

				} else {
					log.Println("Cannot find label for uri: ", uri2)
				}
			}

		} else {
			log.Println("Cannot find label for uri: ", uri1)
		}

	}

	//helpers.PrettyPrint(response.CategoryAmounts)
	for group, facets := range results.FacetCounts.FacetFields {
		if labelgroup, ok := labelsByGroupKeyAndPerma[facetFieldToLabelgroupKey[group]]; ok {
			for i := 0; i < len(facets); i += 2 {
				if lbl, ok := labelgroup[fmt.Sprintf("%v", facets[i])]; ok {
					response.CategoryAmounts[fmt.Sprintf("%v", lbl.ID)] = int(facets[i+1].(float64))
				} else {
					//log.Printf("Could not found count for facet value %v\n", facets[i])
				}
			}
		} else {
			//log.Printf("Could not found counts for facet field %v\n", group)
		}
	}
	//helpers.PrettyPrint(response.CategoryAmounts)
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(response); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func companiesSearchHandler(w http.ResponseWriter, r *http.Request) {
	_, _, _, _, labelsByGroupKeyAndPerma := entities.CachedJobs.GetPositionsAndLabelgroups()
	searchStruct, loc, err := createSolrSearchStruct("Companies", r, labelsByGroupKeyAndPerma, nil)
	if err != nil {
		err = fmt.Errorf("(From %s, %s): %s\nOrig URL: %s", r.Header.Get("X-FORWARDED-FOR"), r.Header.Get("REFERER"), err, r.Header.Get("URL"))
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if searchStruct.Map == 1 {
		searchStruct.Location = ""
		searchStruct.LatLng = ""
	}
	var results solr.SolrQueryCallResultCompanies
	if err := solrCoreCompanies.Search(searchStruct, &results); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if results.Response.NumFound == 0 {
		searchStruct = makeFuzzy(searchStruct)
		if err := solrCoreCompanies.Search(searchStruct, &results); err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
	response := entities.CompanySearchReturn{
		HasMore:         results.Response.NumFound > results.Response.Start+len(results.Response.Docs),
		NumberOfPages:   int(math.Ceil(float64(results.Response.NumFound) / float64(searchStruct.Rows))),
		Companies:       results.Response.Docs,
		RowCount:        results.Response.NumFound,
		Location:        loc,
		CategoryAmounts: map[string]int{},
	}
	if searchStruct.Map == 1 {
		companiesMapResult := []*entities.SolrDocumentCompanyMap{}
		for _, v := range results.Response.Docs {
			companiesMapResult = append(companiesMapResult, &entities.SolrDocumentCompanyMap{
				Id:     v.Id,
				LatLng: v.LatLng,
			})
		}
		response.CompaniesMap = companiesMapResult
	} else {
		response.Companies = results.Response.Docs
	}
	facetFieldToLabelgroupKey := map[string]string{}
	titleTransformer := cases.Title(language.Dutch)
	for labelGroupKey := range labelsByGroupKeyAndPerma {
		camelCase := strings.ReplaceAll(titleTransformer.String(strings.ReplaceAll(strings.ToLower(labelGroupKey), "_", " ")), " ", "")
		facetFieldToLabelgroupKey[camelCase] = labelGroupKey
	}
	facetFieldToLabelgroupKey["Countries"] = "COUNTRY"

	// cats := labelsByGroupKeyAndPerma["CATEGORY"]
	// pivots := results.FacetCounts.FacetPivots["FunctionLevel1,FunctionLevel2,FunctionLevel3"]
	// level := 1
	// for len(pivots) > 0 && level <= 3 {
	// 	nextpivots := []solr.Pivot{}
	// 	for _, pivot := range pivots {
	// 		if lbl, ok := cats[pivot.Value]; ok {
	// 			if lbl.Level == level {
	// 				response.CategoryAmounts[fmt.Sprintf("%v", lbl.ID)] = int(pivot.Count)
	// 			}
	// 		}
	// 		nextpivots = append(nextpivots, pivot.Pivot...)
	// 	}
	// 	level++
	// 	pivots = nextpivots
	// }
	for group, facets := range results.FacetCounts.FacetFields {
		if labelgroup, ok := labelsByGroupKeyAndPerma[facetFieldToLabelgroupKey[group]]; ok {
			for i := 0; i < len(facets); i += 2 {
				if lbl, ok := labelgroup[fmt.Sprintf("%v", facets[i])]; ok {
					response.CategoryAmounts[fmt.Sprintf("%v", lbl.ID)] = int(facets[i+1].(float64))
				} else {
					// log.Printf("Could not find count for facet value %v\n", facets[i])
				}
			}
		} else {
			//log.Printf("Could not find counts for facet field %v\n", group)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(response); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func candidatesSearchHandler(w http.ResponseWriter, r *http.Request) {
	_, labelsByGroupKeyAndID, _, labelsByGroupKeyAndPerma := entities.CachedCompanies.GetLabelgroups()
	searchStruct, loc, err := createSolrSearchStruct("Candidates", r, labelsByGroupKeyAndPerma, labelsByGroupKeyAndID)
	if err != nil {
		err = fmt.Errorf("(From %s, %s): %s\nOrig URL: %s", r.Header.Get("X-FORWARDED-FOR"), r.Header.Get("REFERER"), err, r.Header.Get("URL"))
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if searchStruct.Map == 1 {
		searchStruct.Location = ""
		searchStruct.LatLng = ""
	}
	var results solr.SolrQueryCallResultCandidates
	if err := solrCoreCandidates.Search(searchStruct, &results); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if results.Response.NumFound == 0 {
		searchStruct = makeFuzzy(searchStruct)
		if err := solrCoreCandidates.Search(searchStruct, &results); err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
	response := entities.CandidateSearchReturn{
		HasMore:         results.Response.NumFound > results.Response.Start+len(results.Response.Docs),
		NumberOfPages:   int(math.Ceil(float64(results.Response.NumFound) / float64(searchStruct.Rows))),
		Candidates:      results.Response.Docs,
		RowCount:        results.Response.NumFound,
		Location:        loc,
		CategoryAmounts: map[string]int{},
	}
	if searchStruct.Map == 1 {
		candidatesMapResult := []*entities.SolrDocumentCandidateMap{}
		for _, v := range results.Response.Docs {
			candidatesMapResult = append(candidatesMapResult, &entities.SolrDocumentCandidateMap{
				Id:     v.Id,
				LatLng: v.LatLng,
			})
		}
		response.CandidatesMap = candidatesMapResult
	} else {
		response.Candidates = results.Response.Docs
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
func handleLabels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var requestdata entities.LabelgroupsPostRequest4net
	decoder := json.NewDecoder(r.Body)

	err := decoder.Decode(&requestdata)
	if err != nil {
		if err != io.EOF {
			JSONError(w, r, fmt.Errorf("not found"), http.StatusInternalServerError, true, false)
			return
		}
	}
	var usedKeys = map[string]bool{}
	//['Candidates', 'Jobs', 'Companies', 'JobApplications'
	var returnLabelgroups = []*entities.Labelgroup4net{}
	var joblabelgroups = []*entities.Labelgroup4net{}
	var companylabelgroups = []*entities.Labelgroup4net{}
	if requestdata.LabelIsUsedFor == "" || requestdata.LabelIsUsedFor == "Jobs" {
		_, joblabelgroups, _, _, _ = entities.CachedJobs.GetPositionsAndLabelgroups()
		for _, labelgroup := range joblabelgroups {
			if requestdata.LabelGroupKey == "" || labelgroup.Key == requestdata.LabelGroupKey {
				if _, ok := usedKeys[labelgroup.Key]; !ok {
					returnLabelgroups = append(returnLabelgroups, labelgroup)
					usedKeys[labelgroup.Key] = true
				}
			}
		}
	}
	if requestdata.LabelIsUsedFor == "" || requestdata.LabelIsUsedFor == "Companies" {
		companylabelgroups, _, _, _ = entities.CachedCompanies.GetLabelgroups()
		for _, labelgroup := range companylabelgroups {
			if requestdata.LabelGroupKey == "" || labelgroup.Key == requestdata.LabelGroupKey {
				if _, ok := usedKeys[labelgroup.Key]; !ok {
					returnLabelgroups = append(returnLabelgroups, labelgroup)
					usedKeys[labelgroup.Key] = true
				}
			}
		}
	}

	jsonReturn, err := json.Marshal(entities.LabelgroupsOutput4net{LabelGroups: returnLabelgroups})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonReturn)
}

func handleLabelsForTrans(w http.ResponseWriter, r *http.Request) {
	var usedKeys = map[string]bool{}
	//['Candidates', 'Jobs', 'Companies', 'JobApplications'
	var returnLabelgroups = []*entities.Labelgroup4net{}
	var joblabelgroups = []*entities.Labelgroup4net{}
	var companylabelgroups = []*entities.Labelgroup4net{}
	_, joblabelgroups, _, _, _ = entities.CachedJobs.GetPositionsAndLabelgroups()
	for _, labelgroup := range joblabelgroups {
		if _, ok := usedKeys[labelgroup.Key]; !ok {
			returnLabelgroups = append(returnLabelgroups, labelgroup)
			usedKeys[labelgroup.Key] = true
		}
	}
	companylabelgroups, _, _, _ = entities.CachedCompanies.GetLabelgroups()
	for _, labelgroup := range companylabelgroups {
		if _, ok := usedKeys[labelgroup.Key]; !ok {
			returnLabelgroups = append(returnLabelgroups, labelgroup)
			usedKeys[labelgroup.Key] = true
		}
	}
	translations := map[string]map[string]string{
		"nl": {},
		"en": {},
	}
	for _, labelgroup := range returnLabelgroups {
		translations["nl"][labelgroup.Key] = labelgroup.Title
		translations["en"][labelgroup.Key] = labelgroup.TitleEn
		if labelgroup.Key != "CATEGORY" && labelgroup.Key != "COUNTRY" && labelgroup.Key != "REGION" {
			for _, lbl := range labelgroup.Labels {
				translations["nl"][lbl.Perma] = lbl.Title
				translations["en"][lbl.Perma] = lbl.TitleEn
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(translations)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobsFeed(w http.ResponseWriter, r *http.Request) {
	positions := entities.CachedJobs.GetAllPublishedPositions(false)
	returnJobs := entities.Job2xmlReturn{Jobs: []entities.Job2xmljob{}}
	for _, position := range positions {
		returnJobs.Jobs = append(returnJobs.Jobs, *position.As4net)
	}
	returnJobs.RowCount = len(returnJobs.Jobs)
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(returnJobs)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobsFeedSolr(w http.ResponseWriter, r *http.Request) {
	positions := entities.CachedSolrJobs.GetAllPublished(false)
	resp := struct {
		Jobs     []*entities.SolrDocumentJob
		RowCount int
	}{}
	for _, pos := range positions {
		resp.Jobs = append(resp.Jobs, pos)
	}
	resp.RowCount = len(resp.Jobs)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobsFeedVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]int{
		"version": entities.CachedJobs.GetVersion(),
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobsExternalIDs(w http.ResponseWriter, r *http.Request) {
	toSFID := map[string]string{}
	posids, _ := entities.RetreiveAllPositionIDs()
	for _, ids := range posids {
		toSFID[ids.ExternalID1] = ids.ID
	}
	positions := entities.CachedJobs.GetAllPublishedPositions(true)
	returnJobs := struct {
		CountNoExternalID int
		CountTotalSF      int
		CountTotal4net    int
		NoExternalID      []string
		NotIn4Net         []struct {
			ID4net  string
			IDSF    string
			URLSF   string
			URL4NET string
		}
		NotInSF []struct {
			ID4net  string
			IDSF    string
			URLSF   string
			URL4NET string
		}
		NotInSFBecauseErrors []struct {
			ID4net  string
			IDSF    string
			URLSF   string
			URL4NET string
			Errors  []string
		}
		All4net []string
		AllSF   []string
	}{}

	in4net := map[string]int{}
	insfsoap := map[string]int{}
	cne := 0
	csf := 0
	for _, position := range positions {
		if strings.TrimSpace(position.As4net.ExternalID) == "" {
			returnJobs.NoExternalID = append(returnJobs.NoExternalID, strings.TrimSpace(position.As4net.ID))
			cne++
		} else {
			if len(position.As4net.ValidationErrors) == 0 {
				csf++
				insfsoap[strings.TrimSpace(position.As4net.ExternalID)] = 1
				returnJobs.AllSF = append(returnJobs.AllSF, strings.TrimSpace(position.As4net.ExternalID))
			}
			//toSFID[strings.TrimSpace(position.As4net.ExternalID)] = position.As4net.ID
		}
	}
	returnJobs.CountNoExternalID = cne
	returnJobs.CountTotalSF = csf
	resp, err := http.Get("https://andwork.com/ext/job2xml")
	if err != nil {
		fmt.Println(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("wrong status: %v", resp.StatusCode)
	}
	var byteValue []byte
	byteValue, err = io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
	}
	var jobs struct {
		XMLName xml.Name `xml:"Jobs"`
		Jobs    []struct {
			XMLName   xml.Name `xml:"Job"`
			Reference string   `xml:"Reference"`
		} `xml:"Job"`
	}
	err = xml.Unmarshal(byteValue, &jobs)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	returnJobs.CountTotal4net = len(jobs.Jobs)
	for _, job := range jobs.Jobs {
		in4net[job.Reference] = 1
		returnJobs.All4net = append(returnJobs.All4net, job.Reference)

		if insfsoap[job.Reference] != 1 {

			var valerrors = []string{}
			if idsf, ok := toSFID[job.Reference]; ok {
				if position, ok := positions[idsf]; ok {
					valerrors = position.As4net.ValidationErrors
				}
			}
			if len(valerrors) > 0 {
				data := struct {
					ID4net  string
					IDSF    string
					URLSF   string
					URL4NET string
					Errors  []string
				}{job.Reference, "", "", "", nil}
				if idsf, ok := toSFID[job.Reference]; ok {
					data.IDSF = idsf
					data.URLSF = "https://jobsmediagroup.lightning.force.com/lightning/r/cxsrec__cxsPosition__c/" + idsf + "/view"
					data.URL4NET = "https://vpn-andwork.jmgcrm.nl/Jobs/Details/" + job.Reference
					if position, ok := positions[idsf]; ok {
						data.Errors = position.As4net.ValidationErrors
					}
				}
				returnJobs.NotInSFBecauseErrors = append(returnJobs.NotInSFBecauseErrors, data)
			} else {
				data := struct {
					ID4net  string
					IDSF    string
					URLSF   string
					URL4NET string
				}{job.Reference, "", "", ""}
				if idsf, ok := toSFID[job.Reference]; ok {
					data.IDSF = idsf
					data.URLSF = "https://jobsmediagroup.lightning.force.com/lightning/r/cxsrec__cxsPosition__c/" + idsf + "/view"
					data.URL4NET = "https://vpn-andwork.jmgcrm.nl/Jobs/Details/" + job.Reference
				}
				returnJobs.NotInSF = append(returnJobs.NotInSF, data)
			}

		}
	}
	for id := range insfsoap {
		if in4net[id] != 1 {
			data := struct {
				ID4net  string
				IDSF    string
				URLSF   string
				URL4NET string
			}{id, "", "", ""}
			if idsf, ok := toSFID[id]; ok {
				data.IDSF = idsf
				data.URLSF = "https://jobsmediagroup.lightning.force.com/lightning/r/cxsrec__cxsPosition__c/" + idsf + "/view"
				data.URL4NET = "https://vpn-andwork.jmgcrm.nl/Jobs/Details/" + id
			}
			returnJobs.NotIn4Net = append(returnJobs.NotIn4Net, data)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(returnJobs)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobURLBy4NetID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("URLBy4NetID", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("no id found"), http.StatusNotFound, true, false)
		return
	}
	job, err := entities.CachedJobs.GetPublishedPosition(fragments[0], false, true, false)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	err = json.NewEncoder(w).Encode(map[string]map[string]string{"uris": {"en": job.As4net.PermaEn, "nl": job.As4net.PermaNl}})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func handleJobURLBy4NetPerma(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("URLBy4NetPerma", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("no id found"), http.StatusNotFound, true, false)
		return
	}
	job, err := entities.CachedJobs.GetPublishedPosition(fragments[0], false, false, true)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	err = json.NewEncoder(w).Encode(map[string]interface{}{"uris": map[string]string{"en": job.As4net.PermaEn, "nl": job.As4net.PermaNl}, "id": job.As4net.ID})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handlePublishedJobsID(w http.ResponseWriter, r *http.Request) {
	logError := true
	if r.FormValue("component") == "true" {
		logError = false
	}
	fragments := helpers.GetFragmentsAfter("Published", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	includingInvalid := r.URL.Query().Get("includingInvalid") != ""
	job, err := entities.CachedJobs.GetPublishedPosition(fragments[0], includingInvalid, false, false)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, logError, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Job entities.Job2xmljob
	}{
		Job: *job.As4net,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func handleJobsID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("ID", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	job, err := entities.CachedSolrJobs.Get(fragments[0], false)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Job entities.SolrDocumentJob
	}{
		Job: *job,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handlePublishedJobsIDSolr(w http.ResponseWriter, r *http.Request) {
	logError := true
	if r.FormValue("component") == "true" {
		logError = false
	}
	fragments := helpers.GetFragmentsAfter("Solr", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	includingInvalid := r.URL.Query().Get("includingInvalid") != ""
	job, err := entities.CachedSolrJobs.GetPublished(fragments[0], false, includingInvalid)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, logError, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Job entities.SolrDocumentJob
	}{
		Job: *job,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handlePublishedCompaniesIDSolr(w http.ResponseWriter, r *http.Request) {
	logError := true
	if r.FormValue("component") == "true" {
		logError = false
	}
	fragments := helpers.GetFragmentsAfter("Solr", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	company, err := entities.CachedSolrCompanies.GetPublished(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, logError, false)
		return
	}
	recentJobs := entities.CachedSolrJobs.GetPublishedPositionsByAccountId(company.Id)
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Company    entities.SolrDocumentCompany
		RecentJobs []*entities.AccountJob
	}{
		Company:    *company,
		RecentJobs: recentJobs,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCompaniesFeed(w http.ResponseWriter, r *http.Request) {
	positions := entities.CachedJobs.GetAllPublishedPositions(false)
	posIDsByAccIDs := map[string][]string{}
	for _, pos := range positions {
		posIDsByAccIDs[pos.Full.Account] = append(posIDsByAccIDs[pos.Full.Account], pos.Full.ID)
	}
	companies := entities.CachedCompanies.GetAllPublishedCompany2XML()

	type feed struct {
		Companies []entities.Company2xmlcompany `json:"Companies,omitempty"`
		RowCount  int                           `json:"RowCount,omitempty"`
	}
	returnCompanies := feed{Companies: []entities.Company2xmlcompany{}}
	for _, company := range companies {
		company.JobsCount = len(posIDsByAccIDs[company.ID])
		returnCompanies.Companies = append(returnCompanies.Companies, *company)
	}

	returnCompanies.RowCount = len(returnCompanies.Companies)
	jsonReturn, err := json.Marshal(returnCompanies)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonReturn)
}
func handleCompaniesFeedSolr(w http.ResponseWriter, r *http.Request) {
	positions := entities.CachedSolrJobs.GetAllPublished(false)
	posIDsByAccIDs := map[string][]string{}
	for _, pos := range positions {
		posIDsByAccIDs[pos.CompanyId] = append(posIDsByAccIDs[pos.CompanyId], pos.Id)
	}
	companies := entities.CachedSolrCompanies.GetAllPublished()

	resp := struct {
		Companies []*entities.SolrDocumentCompany
		RowCount  int
	}{}
	for _, company := range companies {
		company.JobsCount = len(posIDsByAccIDs[company.Id])
		resp.Companies = append(resp.Companies, company)
	}

	resp.RowCount = len(resp.Companies)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCompaniesFeedVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]int{
		"version": entities.CachedCompanies.GetVersion(),
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func handleCompanyLogo(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Logo", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	metadata, imgbody, err := entities.CheckAccountLogoCache(fragments[0], false, "")
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", metadata.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%v", len(imgbody)))
	w.Header().Set("Content-Disposition", "inline; filename=\""+metadata.Filename+"\"")
	_, _ = w.Write(imgbody)
}

func handleCompanyTextImage(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("TextImage", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	err := entities.HttpWriteAccountTextImage(w, fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
	}
}

func handleCompaniesID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Company", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	company, err := entities.CachedCompanies.GetCompany(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(company); err != nil {
		JSONError(w, r, fmt.Errorf("could not encode"), http.StatusInternalServerError, true, false)
	}
}

func handleCompanyURLBy4NetID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("URLBy4NetID", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	company := entities.CachedCompanies.GetCompanyPublishedBy4NetId(fragments[0])
	if company == nil {
		JSONError(w, r, fmt.Errorf("could not find id using GetCompanyPublishedBy4NetId %s", fragments[0]), http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"uri": company.Perma}); err != nil {
		JSONError(w, r, fmt.Errorf("could not encode company"), http.StatusInternalServerError, true, false)
		return
	}
}

func handleCompanyURLBy4NetPerma(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("URLBy4NetPerma", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	if id, ok := entities.AccountPermaToID[fragments[0]]; ok {
		comp, err := entities.RetrieveCompany(id)
		if err != nil {
			JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
			return
		}
		if !entities.CanShowCompanyOnline(comp) {
			JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"uri": comp.Perma}); err != nil {
			JSONError(w, r, fmt.Errorf("could not encode company"), http.StatusInternalServerError, true, false)
			return
		}
		return
	}
	comp := entities.CachedCompanies.GetCompanyPublishedBy4NetPerma(fragments[0])
	if comp == nil {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"uri": comp.Perma, "id": comp.ID}); err != nil {
		JSONError(w, r, fmt.Errorf("could not encode company"), http.StatusInternalServerError, true, false)
		return
	}
}

func handlePublishedCompaniesID(w http.ResponseWriter, r *http.Request) {
	logError := true
	if r.FormValue("component") == "true" {
		logError = false
	}
	fragments := helpers.GetFragmentsAfter("Published", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	id := fragments[0]
	comp, err := entities.GetCompany(id)
	if err != nil {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, logError, false)
		return
	}
	if !entities.CanShowCompanyOnline(comp) {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, logError, false)
		return
	}
	recentJobs := entities.CachedJobs.GetPublishedPositionsByAccountId(comp.ID)
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Company    entities.Company2xmlcompany
		RecentJobs []*entities.AccountJob
	}{
		Company:    *comp,
		RecentJobs: recentJobs,
	})
	if err != nil {
		JSONError(w, r, fmt.Errorf("could not encode company or recentjobs"), http.StatusInternalServerError, true, false)
	}
}

func handleAllCompaniesID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("All", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	id := fragments[0]
	comp, err := entities.GetCompany(id)
	if err != nil {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(struct {
		Company entities.Company2xmlcompany
	}{
		Company: *comp,
	})
	if err != nil {
		JSONError(w, r, fmt.Errorf("could not encode company or recentjobs"), http.StatusInternalServerError, true, false)
	}
}

func handleJobApplicationInvite(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Invite", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input entities.AcceptRejectAlreadyInvitedJobApplication
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	if err := entities.JobApplicationInvite(fragments[0], fragments[1], input); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobApplicationReject(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Reject", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input entities.AcceptRejectAlreadyInvitedJobApplication
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	if err := entities.JobApplicationReject(fragments[0], fragments[1], input); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobApplicationAlreadyInvited(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("AlreadyInvited", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input entities.AcceptRejectAlreadyInvitedJobApplication
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	if err := entities.JobApplicationAlreadyInvited(fragments[0], fragments[1], input); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobApplicationAcceptRejectURLBy4NetGUID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("AcceptRejectURLBy4NetGUID", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	if !(cfg.Brand == globals.ANDWORK || cfg.Brand == globals.JOUWICT) {
		JSONError(w, r, fmt.Errorf("we only have urls for andwork and jouwict"), http.StatusBadRequest, true, false)
		return
	}
	acceptRejectURI, err := entities.GetJobApplicationAcceptRejectURLBy4NetGUID(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]string{"uri": acceptRejectURI})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobApplicationByID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("JobApplication", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	jobApps, err := entities.RetrieveJobApplicationsByIDs([]string{fragments[0]})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if jobApp, ok := jobApps[fragments[0]]; !ok {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	} else {
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(map[string]*entities.JobApplication{"jobapplication": jobApp.As4net})
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
}
func handleScheduler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	html := "<table><thead><tr><th>ID</th><th>Operation</th><th>added</th><th>TrialCount</th><th>Next Trial</th><th>Error</th></tr></thead>"
	for id, item := range scheduler.Schedule.GetAll() {
		errors := []string{}
		for _, ve := range item.Errors {
			errors = append(errors, ve.Error+" ("+time.Unix(ve.At/1000, 0).Format("02 Jan 15:04:05")+")")
		}
		html += fmt.Sprintf("<tr><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td></tr>", id, item.Operation, time.Unix(item.AddedAt/1000, 0).Format("02 Jan 15:04:05"), item.TrialCount, time.Unix(item.RunAt/1000, 0).Format("02 Jan 15:04:05"), strings.Join(errors, "<br>"))
	}
	html += "</table>"
	html = `
		<style>
		* {
			font-family: verdana;
			font-size: 14px;
		  }
		  td {
			vertical-align: top;
		  }
		  tr:nth-child(odd) {
			background-color: #eee;
		  }
		  tr:nth-child(even) {
			background-color: #ddd;
		  }
		  tr.irrelevant td{
			opacity: .2;
		  }
		  table {
			width: 100%;
			border-collapse: collapse;
			margin-top:40px
		  }
		  th {
			background: #000;
			color: #fff;
			text-align: left;
		  }
		  </style>
		` + html

	_, _ = w.Write([]byte(html))
}

// handleTodaysOfficeLoggedinUserIDs used by Targetsheet
func handleTodaysOfficeLoggedinUserIDs(w http.ResponseWriter, r *http.Request) {
	userIDs, err := entities.RetrieveTodaysOfficeLoggedinUserIDs()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(userIDs); err != nil {
		log.Println("handleTodaysOfficeLoggedinUserIDs Encode JSON failed", err)
	}
}

// handleActiveUsers used by Targetsheet
func handleActiveUsers(w http.ResponseWriter, r *http.Request) {
	users, err := entities.RetrieveActiveUsers()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(users); err != nil {
		log.Println("handleActiveUsers Encode JSON failed", err)
	}
}

//	func handleJobApplicationQueue(w http.ResponseWriter, r *http.Request) {
//		html := "<table><thead><tr><th>ID</th><th>added</th><th>TrialCount</th><th>Next Trial</th><th>Error</th></tr></thead>"
//		for id, item := range entities.JobApplicationQueue.Queue {
//			html += fmt.Sprintf("<tr><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td></tr>", id, time.Unix(item.AddedAt/1000, 0).Format("02 Jan 15:04:05"), item.TrialCount, time.Unix(item.RunAt/1000, 0).Format("02 Jan 15:04:05"), item.Error)
//		}
//		html += "</table>"
//		html = `
//			<style>
//			* {
//				font-family: verdana;
//				font-size: 14px;
//			  }
//			  td {
//				vertical-align: top;
//			  }
//			  tr:nth-child(odd) {
//				background-color: #eee;
//			  }
//			  tr:nth-child(even) {
//				background-color: #ddd;
//			  }
//			  tr.irrelevant td{
//				opacity: .2;
//			  }
//			  table {
//				width: 100%;
//				border-collapse: collapse;
//				margin-top:40px
//			  }
//			  th {
//				background: #000;
//				color: #fff;
//				text-align: left;
//			  }
//			  </style>
//			` + html
//		w.Header().Set("Content-Type", "text/html")
//		_, _ = w.Write([]byte(html))
//	}
func handleCompanyJobApplications(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("JobApplications", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	jobApps, err := entities.RetreiveJobApplicationsByAccountId(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(jobApps)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCompanyJobs(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Jobs", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	jobs, err := entities.RetrieveJobsFromAccountIDs([]string{fragments[0]})
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(jobs)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func handleCustomerPortalContactLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	contact, err := entities.RetrieveCustomerPortalContactByEmailAndPassword(input.Email, input.Password)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(contact)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}

	/* demo:
	{"user":{"Id":"0032p000033ptcGAAQ","first_name":"Raymond","last_name":"Staak","email":"marc.rabel@bartosz.nl.inactive","account":{"Id":"0012p00003Ndt9RAAR","Title":"Bartosz","Perma":"bartosz-0012p00003Ndt9RAAR","Logo":"/Company/Logo/0012p00003Ndt9RAAR","Intro":"\u003cp\u003eBartosz is een grensverleggende specialist in software testen en testmanagement. Elke dag brengt het team van Bartosz\u0026nbsp;mensen, bedrijven en softwaresystemen v\u0026eacute;rder. Vanuit een onbegrensde drive, kennis van het vak en een voorliefde voor het toevoegen van waarde.\u003c/p\u003e","Zipcode":"3905 NK","City":"Veenendaal","Province":"Utrecht","Country":{"Id":"NL","Name":"Nederland"},"Lat":52.0432518,"Lng":5.561700699999999,"CompanyProfile":"\u003cp\u003e\u003cstrong\u003e\u003cspan\u003eBartosz is een grensverleggende specialist in software testen en testmanagement. Elke dag brengt het team van Bartosz mensen, bedrijven en softwaresystemen vérder. Vanuit een onbegrensde drive, kennis van het vak en een voorliefde voor het toevoegen van waarde. \u003c/span\u003e\u003c/strong\u003e\u003c/p\u003e\n\u003cp\u003e\u003cstrong\u003e\u003cimg title=\"Banner\" src=\"/Company/TextImage/bartosz1.png\" alt=\"Banner\" width=\"670\" height=\"224\"\u003e\u003c/img\u003e\u003c/strong\u003e\u003c/p\u003e\n\u003ch2\u003eOver Bartosz\u003c/h2\u003e\n\u003cp\u003e\u003cspan\u003eHet team van Bartosz werkt vanuit het kantoor in Veenendaal voor de top 250 van Nederlandse bedrijven in onder andere finance, overheid en industrie. Voor hen zetten de toegewijde testconsultants van Bartosz nèt dat stapje extra. Het zijn stuk voor stuk analytisch onderlegde ICT-professionals die communicatief sterk zijn en agility in hun DNA hebben. Theoretische zwaargewichten die nulletjes en eentjes direct kunnen doorvertalen naar de praktijk. Testers die high-impact applicaties en systemen analyseren, risico’s blootleggen en barrières wegnemen met maar één doel: een bijdrage leveren aan de dienstverlening van de klanten. \u003c/span\u003e\u003c/p\u003e\n\u003cp\u003e\u003cimg src=\"/Company/TextImage/bartosz2.png\" width=\"670\" height=\"224\"\u003e\u003c/img\u003e\u003c/p\u003e\n\u003ch2\u003eKlanten en awards\u003c/h2\u003e\n\u003cp\u003eBartosz wil software samen elke dag beter maken. Dat dit de kracht van het bedrijf is, blijkt uit de waardering die klanten hebben uitgesproken. Bartosz is daar dan ook trots op. Een aantal van hun klanten zijn:\u003cbr\u003e\u003cimg title=\"klanten\" src=\"/Company/TextImage/bartoszklanten.png\" alt=\"klanten\" width=\"670\" height=\"224\"\u003e\u003c/img\u003e\u003c/p\u003e\n\u003cp\u003eDe \u003cspan\u003esuccesverhalen van Bartosz hebben de afgelopen jaren extra glans gekregen dankzij de verschillende mooie awards die het bedrijf in ontvangst heeft mogen nemen. Naast de award voor Beste Werknemer wordt Bartosz jaarlijks voor veel andere awards genomineerd: de Best Managed Companies, FD Gazelle Awards, Deloitte Technology Fast 50, High Growth Award, ICT Award Veenendaal en de Financieel Gezond Award door Graydon. Bovendien heeft het bedrijf in 2017 een mooie 5e plek bemachtigd in de Computable Top 100 van meest favoriete ICT-werkgevers.\u003c/span\u003e\u003c/p\u003e\n\u003ch2\u003eWerken bij Bartosz\u003c/h2\u003e\n\u003cp\u003e\u003cspan\u003eDe mensen van Bartosz vormen de drijvende kracht achter het bedrijf. Daarom loopt Bartosz voorop in trainen en opleiden, kent het bedrijf een horizontale organisatiestructuur en wordt er veel waarde gehecht aan een inspirerende interne cultuur vanuit de ‘work hard, play hard’ mentaliteit. Bartosz gelooft dat dit de enige manier is om klanten te kunnen blijven verrassen. En de enige manier om samen het beste te kunnen verbeteren.\u003c/span\u003e\u003c/p\u003e\n\u003cp\u003e\u003cspan\u003eNaast een uitstekend prestatieafhankelijk salaris bij de mooiste en meest ambitieuze testspecialist van Nederland, biedt Bartosz je een aantal zeer aantrekkelijke secundaire arbeidsvoorwaarden. Zo heb jij ongelimiteerde mogelijkheden tot opleidingen en trainingen. Daarnaast wil Bartosz het succes graag met de werknemers delen door middel van winstdeling. Ook krijg jij een leaseauto of reiskostenvergoeding. Daarnaast krijg jij een premievrije ziektekostenverzekering, basis én aanvullend, en tevens een premievrije collectieve pensioenregeling. Buiten werktijd organiseert het team van Bartosz regelmatig leuke uitjes.\u003c/span\u003e\u003c/p\u003e\n\u003cp\u003e\u003cimg title=\"Medewerkers\" src=\"/Company/TextImage/bartoszwerkenbij.png\" alt=\"Medewerkers\" width=\"670\" height=\"224\"\u003e\u003c/img\u003e\u003c/p\u003e\n\u003ch2\u003eWaar zit Bartosz?\u003c/h2\u003e\n\u003cp\u003e\u003c/p\u003e\n\u003cp\u003eDe kantoren van Bartosz zijn gevestigd in Veenendaal en Den Haag. \u003c/p\u003e","labels":[{"Id":52000,"Title":"Provincie","titleEn":"Province","Key":"REGION","Labels":[{"Id":52005,"Title":"NL: Utrecht","titleEn":"NL: Utrecht","Perma":"nl-utrecht","JobsCount":44,"Sorting":0,"visible":false,"level":0,"uriNl":"","uriEn":"","ParentId":0}]}],"street":"Citadel 8","address":{"en":{"city":"Veenendaal","city_area":"","postal_code":"3905 NK","street":"Citadel","house_number":"8","province":"Utrecht","country":"Netherlands","country_code":"NL"},"fr":{"city":"Veenendaal","city_area":"","postal_code":"3905 NK","street":"Citadel","house_number":"8","province":"Utrecht","country":"Pays-Bas","country_code":"NL"},"nl":{"city":"Veenendaal","city_area":"","postal_code":"3905 NK","street":"Citadel","house_number":"8","province":"Utrecht","country":"Nederland","country_code":"NL"}},"address_levenshtein_city":1,"address_levenshtein_street":1,"address_has_housenr":true,"address_location_str":"Citadel  8, , VEENENDAAL, NLD","address_location_geocoded_str":"Citadel 8, , Veenendaal, NL"}}}
	*/
}
func handleCandidateByEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("not allowed %v", r.Method), http.StatusMethodNotAllowed, true, false)
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	candidate, err := entities.RetrieveCandidateByEmail(input.Email)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(candidate.As4net)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateByEmailAndPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	candidate, err := entities.RetrieveCandidateByEmailAndPassword(input.Email, input.Password)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(candidate.As4net)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateStatusByID(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("ByID", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	status, err := entities.RetreiveCandidateStatusByID(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(status)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("not allowed %v", r.Method), http.StatusMethodNotAllowed, true, false)
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	status, err := entities.RetreiveCandidateStatusByEmail(input.Email)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(status)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateValidateEmail(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.ValidateCandidateEmail(input.Email, input.Code)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateFinishRegistration(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CandidateID       string   `json:"candidate_id"`
		RegistrationToken string   `json:"registration_token"`
		SpecialismIDs     []string `json:"specialism_ids"`
		Password          string   `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	emailValidationToken, err := entities.FinishCandidateRegistration(input.CandidateID, input.RegistrationToken, input.SpecialismIDs, input.Password)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": err == nil, "EmailValidationToken": emailValidationToken})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateRequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	_, err = mail.ParseAddress(input.Email)
	if err != nil {
		JSONError(w, r, fmt.Errorf("invalid email: %s", input.Email), http.StatusBadRequest, true, false)
		return
	}
	codeInfo, err := entities.GenerateNewCandidatePWResetCode(input.Email)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(codeInfo)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateResetPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.UpdateCandidatePWWithResetCode(input.Email, input.Code, input.Password)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateUpdatePassword(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("UpdatePassword", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.UpdateCandidatePWWithoutResetCode(fragments[0], input.Password)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateJobApplications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("not allowed %v", r.Method), http.StatusMethodNotAllowed, true, false)
		return
	}
	var input struct {
		CandidateID string `json:"candidate_id"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	jobApplications, err := entities.RetrieveJobApplicationsFromCandidate(input.CandidateID)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(jobApplications)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateUpdateDrivers(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("UpdateDrivers", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input struct {
		Drivers []string `json:"drivers"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.UpdateCandidateDrivers(fragments[0], input.Drivers)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateUpdate(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Update", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.UpdateCandidate(fragments[0], input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateUpdateCV(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("UpdateCV", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	// var input map[string]interface{}
	// err := json.NewDecoder(r.Body).Decode(&input)
	// if err != nil {
	// 	JSONError(w,r, err, http.StatusBadRequest,false)
	// 	return
	// }
	fname := "names-test.pdf"
	fpath := "/home/bart"
	file, err := os.Open(filepath.Join(fpath, fname))
	defer func() { _ = file.Close() }()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	fileHeader := make([]byte, 512)
	if _, err = file.Read(fileHeader); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	resp, err := entities.UpdateCandidateCV(fragments[0], fname, fpath, http.DetectContentType(fileHeader))
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCreateLead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, r, fmt.Errorf("%v not allowed", r.Method), http.StatusMethodNotAllowed, true, false)
		return
	}
	var input entities.LeadPostInput
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		log.Printf("lead error lead post handler: %v", err)
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.PostLead(input, "")
	if err != nil {
		log.Printf("lead error lead post handler: %v", err)
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobsAddresses(w http.ResponseWriter, _ *http.Request) {
	positions := entities.CachedJobs.GetAllPublishedPositions(true)
	positionsSlice := []*entities.PositionStruct{}
	for _, position := range positions {
		position.As4net.AddressLevenshteinStreet = math.Round(position.As4net.AddressLevenshteinStreet*100) / 100
		position.As4net.AddressLevenshteinCity = math.Round(position.As4net.AddressLevenshteinCity*100) / 100
		positionsSlice = append(positionsSlice, position)
	}
	countExternalIDCXSField := 0
	countExternalIDJMGField := 0

	sort.SliceStable(positionsSlice, func(i, j int) bool {

		if !positionsSlice[i].As4net.AddressHasHousenr && positionsSlice[j].As4net.AddressHasHousenr {
			return true
		} else if positionsSlice[i].As4net.AddressHasHousenr && !positionsSlice[j].As4net.AddressHasHousenr {
			return false
		}
		scorei := positionsSlice[i].As4net.AddressLevenshteinCity
		if positionsSlice[i].As4net.AddressLevenshteinStreet < scorei {
			scorei = positionsSlice[i].As4net.AddressLevenshteinStreet
		}
		scorej := positionsSlice[j].As4net.AddressLevenshteinCity
		if positionsSlice[j].As4net.AddressLevenshteinStreet < scorei {
			scorej = positionsSlice[j].As4net.AddressLevenshteinStreet
		}
		if scorei == scorej {
			return positionsSlice[i].As4net.AddressGeocodedStr < positionsSlice[j].As4net.AddressGeocodedStr
		}
		return scorei < scorej
	})

	htmlb := "<table><thead><tr><th>ID/ExternalID</th><th>Recruiter</th><th>Title</th><th>Account Address</th><th>Levenshtein</th><th>GeoResult</th><th>GEO has housenr</th></tr></thead>"
	for _, position := range positionsSlice {
		class := "irrelevant"
		if position.Full.ExternalID != "" {
			countExternalIDCXSField++
			class = ""
		}
		if position.Full.ExternalID1 != "" {
			countExternalIDJMGField++
			class = ""
		}
		if !position.As4net.AddressHasHousenr {
			class += " red"
		} else if position.As4net.AddressLevenshteinStreet < .5 || position.As4net.AddressLevenshteinCity < .6 {
			class += " redorange"
		} else if position.As4net.AddressLevenshteinStreet <= .75 || position.As4net.AddressLevenshteinCity <= .75 {
			class += " orange"
		} else {
			class += " green"
		}
		htmlb += "<tr class='" + class + "'><td>" + position.Full.ID + "<br>" + position.Full.ExternalID1 + "</td><td>" + position.As4net.User.Email + "</td><td><a href='https://jobsmediagroup--full.lightning.force.com/lightning/r/cxsrec__cxsPosition__c/" + position.Full.ID + "/view' target='_blank'>" + position.Full.Name + "</a><br>" + position.Full.AccountName + "</td><td>" + fmt.Sprintf("%v", position.As4net.AddressFromAccount) + "</td><td>" + fmt.Sprintf("street: %.2f<br>city: %.2f", position.As4net.AddressLevenshteinStreet, position.As4net.AddressLevenshteinCity) + "</td><td>" + position.As4net.AddressStr + "<br>" + position.As4net.AddressGeocodedStr + "</td><td>" + fmt.Sprintf("%v", position.As4net.AddressHasHousenr) + "</td></tr>"
	}
	htmlb += "</table>"

	htmlb = fmt.Sprintf("Count: %v<br>Count with ExternalID: %v", len(positions), countExternalIDJMGField) + htmlb
	html := `<!DOCTYPE html>
	<html>
	<head>
		<meta charset="utf-8">
		<style>
		* {
			font-family: verdana;
			font-size: 11px;
		  }
		  thead{
			  position: sticky;
			  top:0;
		  }
		  td,th {
			vertical-align: top;
			padding: 2px 5px;
			text-align:left;
		  }
		  th{
			  vertical-align: bottom;
			  padding: 3px 5px;
		  }
		  tr:nth-child(odd) {
			background-color: #eee;
		  }
		  tr:nth-child(even) {
			background-color: #ddd;
		  }

		  tr.red:nth-child(odd) {
			background-color: #df8a8a;
		  }
		  tr.red:nth-child(even) {
			background-color: #c67474;
		  }

		  tr.redorange:nth-child(odd) {
			background-color: #e8a287;
		  }
		  tr.redorange:nth-child(even) {
			background-color: #db957a;
		  }

		  tr.orange:nth-child(odd) {
			background-color: #f4cf89;
		  }
		  tr.orange:nth-child(even) {
			background-color: #e6bb69;
		  }

		  tr.green:nth-child(odd) {
			background-color: #9fd7a2;
		  }
		  tr.green:nth-child(even) {
			background-color: #87c68a;
		  }
		
		  ttr.irrelevant td{
			opacity: .2;
		  }
		  table {
			width: 100%;
			border-collapse: collapse;
			margin-top:40px
		  }
		  th {
			background: #000;
			color: #fff;
		  }
		  </style>
		</head>
		<body>
			` + htmlb + `
		</body>
	</html>`
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(html))
}
func handleJobsErrors(w http.ResponseWriter, r *http.Request) {
	positions := entities.CachedJobs.GetAllPublishedPositions(true)
	type jobRow struct {
		ID                 string
		Recruiter          string
		Title              string
		ExternalIDCXSField string
		ExternalIDJMGField string
		Errors             []string
		Warnings           []string
	}
	returnJobs := []jobRow{}
	for _, position := range positions {
		if len(position.As4net.ValidationErrors) > 0 || len(position.As4net.ValidationWarnings) > 0 {
			returnJobs = append(returnJobs, jobRow{position.Full.ID, position.As4net.User.Email, position.As4net.Title, position.Full.ExternalID, position.Full.ExternalID1, position.As4net.ValidationErrors, position.As4net.ValidationWarnings})
		}
	}
	sort.SliceStable(returnJobs, func(i, j int) bool {
		return returnJobs[i].Recruiter > returnJobs[j].Recruiter
	})
	countExternalIDCXSField := 0
	countExternalIDJMGField := 0

	baseURL := globals.GetSFBaseURL()

	fragments := helpers.GetFragmentsAfter("Errors", r.URL.Path)
	if len(fragments) > 0 && fragments[0] == "Table" {
		html := `<!DOCTYPE html>
				<html lang="en">
  					<head>
    					<meta charset="utf-8" />
						<style>
						* {
							font-family: verdana;
							font-size: 14px;
						}
						h2:nth-child(1) {
							margin: 0;
						}
						h2 {
							margin: 70px 0 0;
							font-size: 26px;
							border-bottom: 1px solid #000;
						}
						td {
							vertical-align: top;
						}
						tr:nth-child(odd) {
							background-color: #eee;
						}
						tr:nth-child(even) {
							background-color: #ddd;
						}
						tr.irrelevant td{
							opacity: .2;
						}
						table {
							width: 100%;
							border-collapse: collapse;
							
						}
						th {
							background: #000;
							color: #fff;
						}
						</style>
					</head>
					<body>
		`
		html += fmt.Sprintf("Changestream listener is active: %v<br/>", globals.StatusData.Get().IsChangeStreamListenerActive)
		html += "<h2>Job Errors</h2>"
		html += "<table><tr><th>ID/ExternalID</th><th>Recruiter</th><th>Title</th><th>Errors</th></tr>"
		for _, j := range returnJobs {
			class := ""
			if j.ExternalIDCXSField != "" {
				countExternalIDCXSField++
				class = ""
			}
			if j.ExternalIDJMGField != "" {
				countExternalIDJMGField++
				class = ""
			}
			if len(j.Errors) > 0 {
				html += "<tr class='" + class + "'><td>" + j.ID + "<br>" + j.ExternalIDJMGField + "</td><td>" + j.Recruiter + "</td><td><a href='" + baseURL + j.ID + "' target='_blank'>" + j.Title + "</a></td><td><ul><li>" + strings.Join(j.Errors, "</li><li>") + "</li></ul></td></tr>"
			}
		}
		html += "</table>"

		html += "<h2>Job Warnings</h2>"
		html += "<table><tr><th>ID/ExternalID</th><th>Recruiter</th><th>Title</th><th>Warnings</th></tr>"
		for _, j := range returnJobs {
			class := ""
			if j.ExternalIDCXSField != "" {
				countExternalIDCXSField++
				class = ""
			}
			if j.ExternalIDJMGField != "" {
				countExternalIDJMGField++
				class = ""
			}
			if len(j.Warnings) > 0 {
				html += "<tr class='" + class + "'><td>" + j.ID + "<br>" + j.ExternalIDJMGField + "</td><td>" + j.Recruiter + "</td><td><a href='" + baseURL + j.ID + "' target='_blank'>" + j.Title + "</a></td><td><ul><li>" + strings.Join(j.Warnings, "</li><li>") + "</li></ul></td></tr>"
			}
		}
		html += "</table>"

		activePackages := entities.CachedActiveSoldPackages.GetAll()
		jobPackageIDs := entities.CachedJobs.GetPackageIDsMap()

		noJobPackages := []*entities.ActiveSoldPackage{}
		for _, ap := range activePackages {
			if _, ok := jobPackageIDs[ap.Id]; !ok {
				noJobPackages = append(noJobPackages, ap)
			}
		}
		html += "<h2>Packages with no jobs</h2>"
		html += fmt.Sprintf("Count: %v<br/>Total Active Packages: %v", len(noJobPackages), len(activePackages))
		html += "<table>\n<tr><th>ID</th><th>Name</th><th>Product</th><th>Start</th><th>End</th></tr>\n"

		sort.SliceStable(noJobPackages, func(a, b int) bool {
			if noJobPackages[a].EndDate == noJobPackages[b].EndDate {
				return noJobPackages[a].Name < noJobPackages[b].Name
			}
			if noJobPackages[a].EndDate == nil {
				return true
			}
			if noJobPackages[b].EndDate == nil {
				return false
			}
			return noJobPackages[b].EndDate.ToTime().After(*noJobPackages[a].EndDate.ToTime())
		})
		for _, j := range noJobPackages {
			enddate := ""
			if j.EndDate != nil {
				enddate = j.EndDate.Format("02-Jan-2006")
			}
			html += fmt.Sprintf("<tr><td><a target='_blank' href='%s%s'>%s</a></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n", baseURL, j.Id, j.Id, j.Name, j.Product.Name, j.StartDate.Format("02-Jan-2006"), enddate)
		}
		html += "</table>\n"
		html += `</body></html>`

		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	} else {
		jsonReturn, err := json.Marshal(map[string]interface{}{"Count": len(returnJobs), "CountWithExternalIDCXSField": countExternalIDCXSField, "CountWithExternalIDJMGField": countExternalIDJMGField, "Jobs": returnJobs})
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonReturn)
	}
}

func handleAccountsFull(w http.ResponseWriter, _ *http.Request) {
	companies := entities.CachedCompanies.GetAllPublishedCompany2XML()
	totalAmount := len(companies)
	a := 0
	comma := []byte(",")

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("["))
	for _, v := range companies {
		a++
		bytes, _ := json.Marshal(v)
		_, _ = w.Write(bytes)
		if a < totalAmount {
			_, _ = w.Write(comma)
		}
	}
	_, _ = w.Write([]byte("]"))
}

func handlePicklists(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("picklists", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	picklists, err := entities.GetPickLists(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	response := map[string]map[string]string{}
	for _, v := range picklists {
		values := map[string]string{}
		for _, pv := range v.PicklistValues {
			values[pv.Value] = pv.Label
		}
		response[v.Name] = values
	}
	jsonReturn, err := json.Marshal(response)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonReturn)
}
func handleFields(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("fields", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	fields, err := salesforcerest.DescribeRequest(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	response := map[string]string{}
	for _, v := range fields.Fields {
		response[v.Name] = v.Type
	}
	jsonReturn, err := json.Marshal(response)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonReturn)
}
func handleHCSpecialisms(w http.ResponseWriter, r *http.Request) {
	data, err := entities.RetreiveHardCriteriumSpecialisms()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(data)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func handleProxySalesForce(w http.ResponseWriter, r *http.Request) {
	body, err := entities.ProxySalesForce(r)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
func handleGeocode(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Geocode", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	location, err := googlegeocode.GetAddressSearch(fragments[0], []string{"nl"}, true)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(location)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleSiteDomain(w http.ResponseWriter, r *http.Request) {
	siteDomain, err := entities.RetreiveSiteDomain()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]string{"SiteDomain": siteDomain})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleAutocompleteCity(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("City", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	cities := entities.CachedLists.GetCities()
	citiesSorted := levenshtein.SortStrings(cities, fragments[0], 0)
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(citiesSorted)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCities(w http.ResponseWriter, r *http.Request) {
	cities := entities.CachedLists.GetCities()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(cities)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleProvinces(w http.ResponseWriter, r *http.Request) {
	provinces := entities.CachedLists.GetProvinces()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(provinces)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleCandidateEmailCounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if counts, err := entities.RetreiveCandidateEmailCounts(); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
	} else {
		err := json.NewEncoder(w).Encode(counts)
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
}
func handleHashPassword(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("HashPassword", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	pwh, _ := entities.HashPassword(fragments[0])
	_, _ = w.Write([]byte(pwh))
}
func handleGeoIP(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("geoip", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	country, ipType, err := geowhois.FindCountry(fragments[0])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err = json.NewEncoder(w).Encode(map[string]string{"country": country, "iptype": string(ipType)}); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleGeoSearch(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("geosearch", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	result, err := googlegeocode.GetAddressSearch(fragments[0], []string{"nl"}, true)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err = json.NewEncoder(w).Encode(result); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleStatus(w http.ResponseWriter, r *http.Request) {
	newurlparts := []string{}
	for _, v := range strings.Split(strings.Trim(r.URL.Path, "/"), "/") {
		newurlparts = append(newurlparts, v)
		if v == "Status" {
			break
		}
	}
	baseurl := strings.Join(newurlparts, "/")
	fragments := helpers.GetFragmentsAfter("Status", r.URL.Path)
	if len(fragments) == 2 {

		if !(fragments[1] == "on" || fragments[1] == "off") {
			JSONError(w, r, fmt.Errorf("status needs to be 'on' or 'off'"), http.StatusBadRequest, true, false)
			return
		}
		disablestatus := false
		if fragments[1] == "off" {
			disablestatus = true
		}

		if fragments[0] == "JobStream" {
			switchsettings.Settings.JobStreamDisabled = disablestatus
		} else if fragments[0] == "StepStream" {
			switchsettings.Settings.StepStreamDisabled = disablestatus
		} else if fragments[0] == "AccountStream" {
			switchsettings.Settings.AccountStreamDisabled = disablestatus
			if !disablestatus {
				recruiters, err := entities.RetrieveContactsRecruiters()
				if err == nil {
					entities.CachedContacts.UpdateAllRecruiters(recruiters)
				}
				entities.MakePublishedJobsAndAccountsFromJobIDs(nil, false)
			}
		} else if fragments[0] == "ContactStream" {
			switchsettings.Settings.ContactStreamDisabled = disablestatus
		} else if fragments[0] == "GeocodeJobFail" {
			if switchsettings.Settings.GeocodeJobFailDisabled != disablestatus {
				switchsettings.Settings.GeocodeJobFailDisabled = disablestatus
				entities.CachedCompanies.ForceGeoRetry(nil)
				positions := entities.CachedJobs.GetDeepCopyByIDs(nil)
				entities.DoPositionConversion4net(positions, false)
				entities.CachedJobs.UpdatePositions(positions, true)
				entities.CachedJobs.IncreaseVersion()
			}
		} else if fragments[0] == "Leadfeeder" {
			switchsettings.Settings.LeadfeederDisabled = disablestatus
		} else if fragments[0] == "SoldPackageStream" {
			switchsettings.Settings.SoldPackageStreamDisabled = disablestatus
		} else {
			JSONError(w, r, fmt.Errorf("entity needs to be 'JobStream', 'JobApplicationStream', 'AccountStream', 'ContactStream', 'GeocodeJobFail' or 'Leadfeeder'"), http.StatusBadRequest, true, false)
			return
		}
		switchsettings.Settings.Persist()
	}
	if !(len(fragments) == 2 || len(fragments) == 0) {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}

	Status := map[string]struct {
		Enabled bool
		Enable  string
		Disable string
	}{
		"JobStream": {
			Enabled: !switchsettings.Settings.JobStreamDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "JobStream/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "JobStream/off"),
		},
		"AccountStream": {
			Enabled: !switchsettings.Settings.AccountStreamDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "AccountStream/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "AccountStream/off"),
		},
		"ContactStream": {
			Enabled: !switchsettings.Settings.ContactStreamDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "ContactStream/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "ContactStream/off"),
		},
		"GeocodeJobFail": {
			Enabled: !switchsettings.Settings.GeocodeJobFailDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "GeocodeJobFail/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "GeocodeJobFail/off"),
		},
		"Leadfeeder": {
			Enabled: !switchsettings.Settings.LeadfeederDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "Leadfeeder/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "Leadfeeder/off"),
		},
		"SoldPackageStream": {
			Enabled: !switchsettings.Settings.SoldPackageStreamDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "SoldPackageStream/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "SoldPackageStream/off"),
		},
		"StepStream": {
			Enabled: !switchsettings.Settings.StepStreamDisabled,
			Enable:  fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "StepStream/on"),
			Disable: fmt.Sprintf("%s/%s/%s", cfg.Host, baseurl, "StepStream/off"),
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(map[string]interface{}{
		"StreamUpdates": entities.StreamActivityCounts,
		"Status":        Status,
		"FeedVersions": map[string]int{
			"Jobs":      entities.CachedJobs.GetVersion(),
			"Companies": entities.CachedCompanies.GetVersion(),
		},
		"Replay": salesforcestream.ReplayStatus,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleRefresh(w http.ResponseWriter, r *http.Request) {
	log.Println("handleRefresh called")
	recruiters, err := entities.RetrieveContactsRecruiters()
	if err == nil {
		entities.CachedContacts.UpdateAllRecruiters(recruiters)
	} else {
		log.Println("handleRefresh could not RetrieveContactsRecruiters", err)
	}

	if _, err := entities.MakePublishedJobsAndAccountsFromJobIDs(nil, false); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err = json.NewEncoder(w).Encode(map[string]bool{
		"success": true,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	log.Println("handleRefresh completed successfully")
}
func handleJobApplicationCVTestHash(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Hash", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	err := entities.TestCVHash(fragments[0], fragments[1])
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err = json.NewEncoder(w).Encode(map[string]bool{
		"success": true,
	})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleJobApplicationCVAuthReHash(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("ReHash", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	err = entities.FindAndReNewCVHash(input.Email, fragments[0], fragments[1])
	if err != nil {
		JSONError(w, r, err, http.StatusBadRequest, true, false)
		return
	}
	jobApps, err := entities.RetrieveJobApplicationsByIDs([]string{fragments[0]})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	if jobApp, ok := jobApps[fragments[0]]; !ok {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	} else {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(map[string]*entities.JobApplication{"jobapplication": jobApp.As4net})
		if err != nil {
			JSONError(w, r, err, http.StatusInternalServerError, true, false)
			return
		}
	}
}
func handleJobApplicationCV(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("CV", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	err := entities.CVFromJobApplication(w, fragments[0], fragments[1], false)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
}
func handleJobApplicationCVExists(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("CVExists", r.URL.Path)
	if len(fragments) != 2 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	err := entities.CVFromJobApplication(w, fragments[0], fragments[1], true)
	if err != nil {
		JSONError(w, r, err, http.StatusNotFound, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func handleRescheduleNonsendJobApplicationEmails(w http.ResponseWriter, r *http.Request) {
	ids, err := entities.RetrieveJobApplicationIDsOfNotSendEmailsInLastWeek()
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	jobAppIdsWithStatusses := map[string]*entities.QueueItemJobApplicationCreate{}
	for _, id := range ids {
		jobAppIdsWithStatusses[id] = &entities.QueueItemJobApplicationCreate{
			JobApplicationID: id,
			Status:           "manually triggered email",
			Action:           "all 'non send' of last week",
		}
	}
	err = entities.HandleNewJobApplications(jobAppIdsWithStatusses)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	result := map[string]interface{}{"success": true, "rescheduled": ids}
	json.NewEncoder(w).Encode(result)
}
func handleRunJobappManually(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("Master", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	jobAppIdWithStatus := map[string]*entities.QueueItemJobApplicationCreate{
		fragments[0]: {
			JobApplicationID: fragments[0],
			Status:           "manually triggered email",
			Action:           "per jobAppID",
		},
	}
	err := entities.HandleNewJobApplications(jobAppIdWithStatus)
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err = json.NewEncoder(w).Encode(map[string]string{"msg": "id " + fragments[0] + " has been added to the JobAppQueue (/sf/JobApplication/Queue)"})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}
func saveJobHash(w http.ResponseWriter, r *http.Request) {
	fragments := helpers.GetFragmentsAfter("SaveJobHashTest", r.URL.Path)
	if len(fragments) != 1 {
		JSONError(w, r, fmt.Errorf("not found"), http.StatusNotFound, true, false)
		return
	}
	ids := []string{fragments[0]}
	if err := entities.AddHashesToNewJobApplications(ids); err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(map[string]bool{"success": true})
	if err != nil {
		JSONError(w, r, err, http.StatusInternalServerError, true, false)
		return
	}
}

func getRequestID(r *http.Request) string {
	if reqID := r.Header.Get("RequestID"); reqID != "" {
		return reqID
	}
	if reqID := r.URL.Query().Get("RequestID"); reqID != "" {
		return reqID
	}
	if reqID := r.Form.Get("RequestID"); reqID != "" {
		return reqID
	}
	return helpers.NRToLetters(1000000 + rand.Intn(9999999-1000000))
}

func JSONOutput(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// JSONError a Helper to write a standardized error as JSON.
func JSONError(w http.ResponseWriter, r *http.Request, err error, code int, logError bool, logStackTrace bool) {
	requestID := getRequestID(r)
	if logError {
		log.Printf("(%s) %v URL: %s", requestID, err, r.RequestURI)
	}
	if logStackTrace {
		log.Printf("Stacktrace from error: %s", debug.Stack())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("%s: %v", requestID, err)})
}
func HTMLError(w http.ResponseWriter, r *http.Request, err error, code int) {
	requestID := getRequestID(r)
	log.Printf("(%s) %v\n\nUrl: %s\n\nStacktrace from error: \n%s\n", requestID, err, r.RequestURI, debug.Stack())
	w.WriteHeader(code)
	_, _ = w.Write([]byte(fmt.Sprintf("(%s) %v", requestID, err)))
}
func Pagination(totalpages uint16, currentpage uint16, urltemplate string) template.HTML {
	if totalpages < 1 {
		totalpages = 1
	}
	currentpageround10 := (currentpage / 10) * 10
	currentpageround100 := (currentpage / 100) * 100
	nrstoshowmap := map[uint16]bool{
		1:                         true,
		totalpages:                true,
		currentpage:               true,
		currentpage - 1:           true,
		currentpage - 2:           true,
		currentpage + 1:           true,
		currentpage + 2:           true,
		currentpageround10:        true,
		currentpageround10 - 10:   true,
		currentpageround10 - 20:   true,
		currentpageround10 + 10:   true,
		currentpageround10 + 20:   true,
		currentpageround100:       true,
		currentpageround100 - 100: true,
		currentpageround100 + 100: true,
	}
	nrstoshowslice := []int{}
	for nr := range nrstoshowmap {
		if nr > 0 && nr <= totalpages {
			nrstoshowslice = append(nrstoshowslice, int(nr))
		}
	}
	sort.Ints(nrstoshowslice)

	type StartEnd struct {
		Start int
		End   int
	}
	btngroups := []StartEnd{}
	var group StartEnd
	var prevNr int
	for idx, nr := range nrstoshowslice {
		if idx == 0 {
			prevNr = nr
			group = StartEnd{Start: nr}
			continue
		}
		if nr-1 != prevNr {
			group.End = prevNr
			btngroups = append(btngroups, group)
			group = StartEnd{Start: nr}
		}
		prevNr = nr
	}
	group.End = prevNr
	btngroups = append(btngroups, group)

	output := []string{}
	if currentpage <= 1 {
		output = append(output, `<span class='paginationbtn pb_first'>&lt;</span>`)
	} else if currentpage > 1 {
		output = append(output, fmt.Sprintf(`<a class='paginationbtna pb_first' href='%s'><span class='paginationbtn'>&lt;</span></a>`, fmt.Sprintf(urltemplate, currentpage-1)))
	}

	btngroupsoutput := []string{}
	for _, btngroup := range btngroups {
		grouphtml := ""
		for nr := btngroup.Start; nr <= btngroup.End; nr++ {
			if nr == int(currentpage) {
				grouphtml += fmt.Sprintf(`<span class='paginationbtn pb_current'>%v</span>`, nr)
			} else {
				grouphtml += fmt.Sprintf(`<a class='paginationbtna' href='%s'><span class='paginationbtn'>%v</span></a>`, fmt.Sprintf(urltemplate, nr), nr)
			}
		}
		btngroupsoutput = append(btngroupsoutput, grouphtml)
	}
	output = append(output, strings.Join(btngroupsoutput, "<span class='paginationbtndivider'>&mldr;</span>"))
	if currentpage >= totalpages {
		output = append(output, `<span class='paginationbtn pb_last'>&gt;</span>`)
	} else if currentpage < totalpages {
		output = append(output, fmt.Sprintf(`<a class='paginationbtna pb_last' href='%s'><span class='paginationbtn'>&gt;</span></a>`, fmt.Sprintf(urltemplate, currentpage+1)))
	}
	return template.HTML(strings.Join(output, ""))
}
