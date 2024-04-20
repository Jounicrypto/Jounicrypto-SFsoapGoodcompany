package config

import "sfsoap/globals"

type AppConfig struct {
	AppMode       globals.Environment
	Brand         globals.Brand
	BindIP        string
	HTTPPort      uint16
	UDSPath       string
	Username      string
	Password      string
	SecurityToken string

	ConsumerKey    string
	ConsumerSecret string

	EndpointURIREST string
	URLLoginREST    string

	LogDir            string
	PersistentDataDir string
	CacheDir          string

	Host     string
	EventURL string

	PreviousProcessToKill string

	StartupDisableJobStream            bool
	StartupDisableJobApplicationStream bool
	StartupDisableAccountStream        bool
	StartupDisableContactStream        bool
	StartupDisableGeocodeJobFail       bool

	DbHost string
	DbPort uint16
	DbUser string
	DbName string
	DbPass string

	UseSolr          bool
	UseSolrCandidate bool
	SolrBaseURL      string

	JavaBinPath         string
	TikaJarPath         string
	TikaOCRConfigPath   string
	TikaNOOCRConfigPath string

	RemoteFileStorageServer string

	ClamdConnectionString string

	JWTLoginPrivateKeyPath string
}

func DefaultConfig(_ string) *AppConfig {
	return &AppConfig{
		AppMode:                            "dev",
		Brand:                              "",
		BindIP:                             "127.0.0.1",
		HTTPPort:                           8080,
		UDSPath:                            "@/tmp/sfsoap",
		Username:                           "",
		Password:                           "",
		SecurityToken:                      "",
		ConsumerKey:                        "",
		ConsumerSecret:                     "",
		EndpointURIREST:                    "",
		URLLoginREST:                       "",
		LogDir:                             "./log",
		PersistentDataDir:                  "./persistentdata",
		CacheDir:                           "./filescache",
		Host:                               "",
		EventURL:                           "",
		PreviousProcessToKill:              "",
		StartupDisableJobStream:            false,
		StartupDisableJobApplicationStream: false,
		StartupDisableAccountStream:        false,
		StartupDisableContactStream:        false,
		StartupDisableGeocodeJobFail:       false,
		DbHost:                             "localhost",
		DbPort:                             3306,
		DbUser:                             "",
		DbName:                             "",
		DbPass:                             "",
		UseSolr:                            false,
		UseSolrCandidate:                   false,
		SolrBaseURL:                        "http://127.0.0.1:8983/solr/",

		JavaBinPath:         "java",
		TikaJarPath:         "tika-app-2.9.0.jar",
		TikaOCRConfigPath:   "tika-ocr-config.xml",
		TikaNOOCRConfigPath: "tika-no-ocr-config.xml",

		RemoteFileStorageServer: "http://127.0.0.1:8081",

		ClamdConnectionString:  "tcp://dev.andwork.com:3368",
		JWTLoginPrivateKeyPath: "",
	}
}
