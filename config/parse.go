package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"strings"
)

//ParseConfig is a default implementation of parsing a generic database based application configuration. When a configuration file is not found, a new one
//will be created.
func ParseConfig(ownName string) (*AppConfig, error) {
	if ownName == "" {
		return nil, errors.New("no ownName supplied to ParseConfig")
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("cannot find configuration directory %v", err)
	}
	_, err = os.Stat(configDir)
	if err != nil {
		configDir, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot find configuration directory - attempted userhome directory too %v", err)
		}
		configDir = configDir + "/.config"
		if err = os.Mkdir(configDir, 0700); err != nil {
			return nil, fmt.Errorf("cannot find configuration directory, cannot create .config in homedir %v", err)
		}
	}

	configDir = configDir + "/" + ownName
	if _, err = os.Stat(configDir); err != nil {
		if err = os.Mkdir(configDir, 0700); err != nil {
			return nil, fmt.Errorf("cannot find configuration directory, cannot create %s in homedir/.config %v", configDir, err)
		}
	}

	configFile := configDir + "/" + ownName + ".config"
	if _, err = os.Stat(configFile); err != nil {
		log.Println("Generating new config file")
		if err = GenDefaultConfig(ownName, configDir, ownName+".config"); err != nil {
			return nil, fmt.Errorf("cannot write default configuration %s ; %v", configFile, err)
		}
	}

	return ParseConfigFromFile(ownName, configFile)
}

// ParseConfigFromFile need to specify the location where the config file is to be expected.
func ParseConfigFromFile(ownName, filePath string) (*AppConfig, error) {

	log.Println("config file: " + filePath)
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not open file %s ; %v", filePath, err)
	}
	defer func() { _ = f.Close() }()

	cfg := DefaultConfig(ownName)

	s := bufio.NewReader(f)
	var line []byte
	for line, _, err = s.ReadLine(); err == nil; line, _, err = s.ReadLine() {
		str := strings.TrimSpace(string(line))
		if strings.HasPrefix(str, "#") {
			continue
		}
		strs := strings.Split(str, "#")
		if len(strs) > 1 {
			str = strs[0]
		}
		parts := strings.SplitN(str, ":", 2)
		if len(parts) > 1 {
			if ok := mapConfigLine(cfg, parts); !ok {
				log.Printf("WARN: could not recognize configuration line: %s\n", str)
			}
		}
	}

	return cfg, nil
}

func mapConfigLine(cfg *AppConfig, parts []string) bool {
	e := reflect.TypeOf(*cfg)
	configKey := strings.ToLower(strings.TrimSpace(parts[0]))
	for walk, mLen := 0, e.NumField(); walk < mLen; walk++ {
		f := e.Field(walk)
		if strings.ToLower(f.Name) == configKey {
			cfgVal := reflect.ValueOf(*cfg)
			t := cfgVal.Field(walk).Type().Kind()
			if t == reflect.String {
				var s string
				if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%s", &s); err != nil {
					log.Printf("WARN: could not parse config key %s val %s ; %v", configKey, parts[1], err)
				}
				reflect.ValueOf(cfg).Elem().Field(walk).SetString(s)
			} else if t == reflect.Uint16 {
				var val uint64
				if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &val); err != nil {
					log.Printf("WARN: could not parse config key %s val %s ; %v", configKey, parts[1], err)
				}
				reflect.ValueOf(cfg).Elem().Field(walk).SetUint(val)
			} else if t == reflect.Bool {
				if strings.ToLower(strings.TrimSpace(parts[1])) == "true" {
					reflect.ValueOf(cfg).Elem().Field(walk).SetBool(true)
				}
			}
			break
		}
	}
	return true
}

func strToBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func WriteConfig(cfg *AppConfig, dir, fName string) error {
	f, err := ioutil.TempFile(dir, fName)
	if err != nil {
		return fmt.Errorf("could not write tempfile in %s ; %v", dir, err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	v := reflect.Indirect(reflect.ValueOf(cfg))
	typeOfS := v.Type()

	var buf bytes.Buffer
	buf.Write([]byte("# sfsoap config file version 1.0 ; bart@jobsmediagroup.com\n"))
	for i := 0; i < v.NumField(); i++ {
		fmt.Printf("Field: %s\tValue: %v\n", strings.ToLower(typeOfS.Field(i).Name), v.Field(i).Interface())
		buf.Write([]byte(fmt.Sprintf("%s:%v\n", strings.ToLower(typeOfS.Field(i).Name), v.Field(i).Interface())))
	}

	if _, err = f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("could not write config %s ; %v", f.Name(), err)
	}

	if err = os.Rename(f.Name(), dir+"/"+fName); err != nil {
		return fmt.Errorf("could not move config file from %s to %s ; %v", f.Name(), dir+"/"+fName, err)
	}
	if err = os.Chmod(dir+"/"+fName, 0600); err != nil {
		return fmt.Errorf("could not chmod config file %s to 0600 ; %v", dir+"/"+fName, err)
	}
	return nil
}

func GenDefaultConfig(ownName, dir, fName string) error {
	cfg := DefaultConfig(ownName)
	err := WriteConfig(cfg, dir, fName)
	return err
}
