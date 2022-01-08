package main

import (
	"os"

	"gopkg.in/yaml.v2"
)

// client config struct
type ClientConnConfig struct {
	Dial      string `yaml:"dial"`
	CaCert    string `yaml:"ca_cert,omitempty"` // to verify server
	MyCert    string `yaml:"my_cert,omitempty"` // for mutual tls
	MyCertKey string `yaml:"my_key,omitempty"`  // for mutual tls
}
type ServerConnConfig struct {
	Listen    string `yaml:"listen"`
	CaCert    string `yaml:"ca_cert,omitempty"` // to verify clients
	MyCert    string `yaml:"my_cert,omitempty"` // server public cert
	MyCertKey string `yaml:"my_key,omitempty"`  // server cert key
}

type AppConfig struct {
	Clients   []ClientConnConfig `yaml:"clients"`
	Server    ServerConnConfig   `yaml:"server"`
	Blacklist []string           `yaml:"blacklist,omitempty"`
}

func ReadConfig(filename string) (*AppConfig, error) {
	var appConfig AppConfig
	cfgFile, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(cfgFile, &appConfig)
	if err != nil {
		return nil, err
	}

	return &appConfig, nil
}
