##Go package for setting up an application's configuration needs

This package can be used to bootstrap a simple server

Right now, we have no tests.

To use it, copy the config directory to your project and then you can do this:

```go
package main

import (
	"log"
	"os"
	"path/filepath"
	"yourpackage/config"
	"yourpackage/jmgstartserver"
)

func main() {
	cfg, err := config.ParseConfig(filepath.Base(os.Args[0]))
	if err != nil {
		log.Fatal("could not parse config ", err)
	}
	srv := jmgstartserver.NewJmgServer(cfg.OwnName, cfg.HttpPort, true)
	// define your http.HandleFunc's here
	srv.Start()
}
```
This will have defaults, but you probably want to change them. Do this by changing the
configuration file that will be generated for you.

After the ParseConfig call, you will receive an AppConfig structure, which is defined like this:
```go
package config

type AppConfig struct {
    HttpPort     uint16
    DbPort       uint16
    Dev          bool
    CreateTables bool
    DropTables   bool
    DbUser       string
    DbPass       string
    DbHost       string
    DbName       string
    OwnName      string
    LogDir       string
}
```
If you want to change the values of the configuration, do not forget to look into ```go
func DefaultConfig(ownName string) *AppConfig``` too. If a file cannot be found at the default location by ```go
ParseConfig``` then a new file will be created with default values will be written automatically.

The ParseConfig has an equivalent ```go
func ParseConfigFromFile(filePath, ownName string) (*AppConfig, error)```  that can be used to specify a filepath to a config to use, this might be usefull
to combine with startup flags, etc..

This package should work with Windows, although it has not yet been tried on that platform.