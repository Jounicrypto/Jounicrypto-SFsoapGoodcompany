package main

import (
	"log"
	"net/http"
	"sfsoap/entities"
	"sfsoap/entities/googlegeocode"
	"sfsoap/globals"
	"sfsoap/helpers"
	"sfsoap/solr"

	"fmt"
	"strconv"
	"strings"
)

func createSolrSearchStruct(entity string, r *http.Request, labelsByGroupKeyAndPerma map[string]map[string]*entities.Label4net, labelsByGroupKeyAndID map[string]map[string]*entities.Label4net) (solr.Search, *googlegeocode.Location, error) {
	searchForm := solr.Search{
		SearchString: "",
		Location:     "",
		LatLng:       "",
		Distance:     5,
		Rows:         20,
		PageNr:       1,
		Filters:      map[string][]string{},
		Map:          0,
		Sort:         "",
	}
	mapSearch, err := strconv.Atoi(r.FormValue("map"))
	if err != nil {
		mapSearch = 0
	}
	searchForm.Map = mapSearch
	if r.FormValue("rows") != "" {
		if r, err := strconv.Atoi(r.FormValue("rows")); err == nil {
			if r > 0 {
				searchForm.Rows = r
			}
		}
	}
	if mapSearch == 1 {
		searchForm.Rows = 999999
	}
	if strings.TrimSpace(r.FormValue("search")) != "" {
		str := helpers.ConvertDiatrics(strings.ToLower(r.FormValue("search")))
		str = solr.RegExps.Alphanumspacedot.ReplaceAllString(str, " ")
		str = solr.RegExps.NumberInBoundary.ReplaceAllString(str, "")
		str = solr.RegExps.Spaces.ReplaceAllString(str, " ")
		str = strings.TrimSpace(str)
		searchForm.SearchString = str
	}

	fragments := helpers.GetFragmentsAfter("Search", r.URL.Path)

	switch entity {
	case "Jobs":
		if len(fragments) > 4 {
			return searchForm, nil, fmt.Errorf("too many uri fragments")
		}
		if r.FormValue("sort") != "" {
			sp := strings.Split(strings.ToLower(r.FormValue("sort")), " ")
			if sp[0] == "date" {
				searchForm.Sort = "PublicationDate"
				if len(sp) > 1 {
					if sp[1] == "desc" {
						searchForm.Sort += " desc"
					} else {
						searchForm.Sort += " asc"
					}
				}
			}
		}
		var lang = ""
		for _, fragment := range fragments {
			for _, lbl := range labelsByGroupKeyAndPerma["CATEGORY"] {
				if lang == "" && (fragment == lbl.URIEn || fragment == lbl.URINl) && lbl.URIEn != lbl.URINl {
					lang = "nl"
					if lbl.URIEn == fragment {
						lang = "en"
					}
					break
				}
			}
		}

		var labelPerFullPerma map[string]*entities.Label4net
		if lang == "nl" {
			labelPerFullPerma = entities.CachedJobs.GetFullURINLToLabel()
		} else {
			labelPerFullPerma = entities.CachedJobs.GetFullURIENToLabel()
		}
		urif := []string{}
		for i, fragment := range fragments {
			urif = append(urif, fragment)
			if foundLabel, ok := labelPerFullPerma[strings.Join(urif, "/")]; ok {
				searchForm.Filters[fmt.Sprintf("FunctionLevel%d", i+1)] = []string{foundLabel.Perma}
			} else if searchForm.Location == "" {
				searchForm.Location = fragment
			} else {
				return searchForm, nil, fmt.Errorf("unknown functionlevels in uri")
			}
		}

		// for i, fragment := range fragments {
		// 	fragment = strings.ToLower(fragment)
		// 	if i < 3 {
		// 		foundLabel := &entities.Label4net{}
		// 		for _, lbl := range labelsByGroupKeyAndPerma["CATEGORY"] {
		// 			if lbl.ParentID == parentID {
		// 				if (lang == "" && (fragment == lbl.URIEn || fragment == lbl.URINl)) || (lang == "nl" && fragment == lbl.URINl) || (lang == "en" && fragment == lbl.URIEn) {
		// 					if lang == "" && lbl.URIEn != lbl.URINl {
		// 						lang = "nl"
		// 						if lbl.URIEn == fragment {
		// 							lang = "en"
		// 						}
		// 					}
		// 					foundLabel = lbl
		// 					parentID = lbl.ID
		// 					break
		// 				}
		// 			}
		// 		}
		// 		if foundLabel.Perma != "" {
		// 			searchForm.Filters[fmt.Sprintf("FunctionLevel%d", i+1)] = []string{foundLabel.Perma}
		// 			continue
		// 		}
		// 	}
		// 	if searchForm.Location != "" {
		// 		return searchForm, nil, fmt.Errorf("unknown functionlevels in uri")
		// 	} else {
		// 		searchForm.Location = fragment
		// 	}
		// }
	case "Companies":
		if len(fragments) > 1 {
			return searchForm, nil, fmt.Errorf("too many uri fragments")
		}
		if len(fragments) == 1 {
			searchForm.Location = fragments[0]
		}
	case "Candidates":
		searchForm.SearchString = r.FormValue("search")
		searchForm.Location = r.FormValue("location")
		if strings.TrimSpace(r.FormValue("competencies")) != "" {
			competencies := strings.Split(r.FormValue("competencies"), ",")
			searchForm.Filters["competencies"] = competencies
			extraSearchStrings := []string{}
			catLabels := labelsByGroupKeyAndID["CATEGORY"]
			for _, cID := range competencies {
				if lbl, ok := catLabels[cID]; ok {
					extraSearchStrings = append(extraSearchStrings, lbl.Title, lbl.TitleEn)
				}
			}
			searchForm.SearchString += " " + strings.Join(extraSearchStrings, " ")
		}
	default:
		return solr.Search{}, nil, fmt.Errorf("unknown entity")
	}

	searchForm.Location = helpers.CleanInput(solr.RegExps.Alphanum, searchForm.Location)
	for _, filter := range strings.Split(r.FormValue("filters"), "~") {
		f := strings.Split(filter, ":")
		if len(f) != 2 {
			continue
		}
		labelgroupKey := strings.ToLower(f[0])
		labelPermas := strings.Split(f[1], ".")

		if lblGrp, ok := labelsByGroupKeyAndPerma[strings.ToUpper(labelgroupKey)]; ok {
			for _, labelPerma := range labelPermas {
				labelPerma = strings.ToLower(labelPerma)

				if _, ok := lblGrp[labelPerma]; ok {
					if _, ok := searchForm.Filters[labelgroupKey]; !ok {
						searchForm.Filters[labelgroupKey] = []string{}
					}
					searchForm.Filters[labelgroupKey] = append(searchForm.Filters[labelgroupKey], labelPerma)
				}
			}
		}
	}
	loc, err := googlegeocode.GetAddressSearch(searchForm.Location, []string{"nl"}, true)
	if err == nil {
		searchForm.LatLng = fmt.Sprintf("%v,%v", loc.Lat, loc.Lng)
	}
	if strings.ToLower(searchForm.Location) == "made" {
		log.Println("Zoeken naar 'made'", loc.Lat, loc.Lng)
	}
	if in := r.FormValue("distance"); in != "" {
		if distance, err := strconv.Atoi(in); err == nil {
			searchForm.Distance = distance
		}
	}
	if in := r.FormValue("pagenr"); in != "" {
		if pagenr, err := strconv.Atoi(in); err == nil {
			if pagenr > 1 {
				searchForm.PageNr = pagenr
			}
		}
	}
	if cfg.Brand == globals.JOUWICT {
		site := r.FormValue("site")
		if site != "" && site != "ICT" {
			val, ok := entities.JouwictSubsites[site]
			if ok {
				if entity == "Jobs" {
					searchForm.Filters["onsubsite"] = []string{val}
				} else if entity == "Companies" {
					searchForm.Filters[site+"FROM"] = []string{"[* TO NOW/DAY]"}
					searchForm.Filters[site+"TILL"] = []string{"[NOW/DAY TO *]"}
				}
			}
		}
	}
	//helpers.PrettyPrint(searchForm)
	return searchForm, loc, nil
}

func updateSolr() {
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

			if cfg.UseSolrCandidate {
				candidates, err := entities.RetrieveCandidatesForSolrSinceLastModified(nil)
				if err != nil {
					log.Println(err)
				} else {
					if err := solrCoreCandidates.UpdateDocs(candidates); err != nil {
						log.Println(err)
					}
				}
			}
		}
	}
}
