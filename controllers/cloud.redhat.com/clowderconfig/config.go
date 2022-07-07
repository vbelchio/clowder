package clowderconfig

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/fsnotify/fsnotify"
)

type ClowderConfig struct {
	Images struct {
		MBOP           string `json:"mbop"`
		Caddy          string `json:"caddy"`
		Keycloak       string `json:"Keycloak"`
		Mocktitlements string `json:"mocktitlements"`
	} `json:"images"`
	DebugOptions struct {
		Logging struct {
			DebugLogging bool `json:"debugLogging"`
		}
		Trigger struct {
			Diff bool `json:"diff"`
		} `json:"trigger"`
		Cache struct {
			Create bool `json:"create"`
			Update bool `json:"update"`
			Apply  bool `json:"apply"`
		} `json:"cache"`
		Pprof struct {
			Enable  bool   `json:"enable"`
			CpuFile string `json:"cpuFile"`
		} `json:"pprof"`
	} `json:"debugOptions"`
	Features struct {
		CreateServiceMonitor        bool `json:"createServiceMonitor"`
		DisableWebhooks             bool `json:"disableWebhooks"`
		WatchStrimziResources       bool `json:"watchStrimziResources"`
		UseComplexStrimziTopicNames bool `json:"useComplexStrimziTopicNames"`
		EnableAuthSidecarHook       bool `json:"enableAuthSidecarHook"`
		KedaResources               bool `json:"enableKedaResources"`
		PerProviderMetrics          bool `json:"perProviderMetrics"`
		ReconciliationMetrics       bool `json:"reconciliationMetrics"`
		DisableCloudWatchLogging    bool `json:"disableCloudWatchLogging"`
		EnableExternalStrimzi       bool `json:"enableExternalStrimzi"`
	} `json:"features"`
}

func watchConfig(config string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Remove == fsnotify.Remove {
					fmt.Printf("config file [%s] modified exiting with [%s]:", config, event.Op)
					os.Exit(127)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Println("error in watching: ", err)
			}
		}
	}()

	err = watcher.Add(config)
	if err != nil {
		panic(err)
	}
	<-done
}

func getConfig() ClowderConfig {
	configPath := "/config/clowder_config.json"

	if path := os.Getenv("CLOWDER_CONFIG_PATH"); path != "" {
		configPath = path
		go watchConfig(configPath)
	}

	fmt.Printf("Loading config from: %s\n", configPath)

	jsonData, err := ioutil.ReadFile(configPath)

	if err != nil {
		fmt.Printf("Config file not found\n")
		return ClowderConfig{}
	}

	clowderConfig := ClowderConfig{}
	err = json.Unmarshal(jsonData, &clowderConfig)

	if err != nil {
		fmt.Printf("Couldn't parse json:\n" + err.Error())
		return ClowderConfig{}
	}

	return clowderConfig
}

var LoadedConfig ClowderConfig

func init() {
	LoadedConfig = getConfig()
}
