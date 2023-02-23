package gsnow

import (
	"fmt"

	"github.com/braintree/heckler/internal/cmplugins/common"
	"gopkg.in/yaml.v3"
)

type GSnow struct {
	Config *GSnowConfig
}

type GSnowConfig struct {
	URL string `yaml:"url"`
}

func (g *GSnow) CMCloseTicket(ticket string, CloseCode string) error {
	return nil
}

func (g *GSnow) CMCreateTicket() (string, error) {
	return "", nil
}

// InitTBStop initializes and returns TBStop object
func InitGSnow() (*GSnow, error) {
	conf, err := getGSnowConfig()
	if err != nil {
		return nil, fmt.Errorf("getting gsnow config: %w", err)
	}

	gsnow := new(GSnow)
	gsnow.Config = conf

	return gsnow, nil
}

// getGSnowConfig reads gsnow configuration
func getGSnowConfig() (*GSnowConfig, error) {
	data, err := common.ReadConfig("gsnow_conf.yaml")
	if err != nil {
		return nil, fmt.Errorf("cannot read config: %w", err)
	}

	conf := new(GSnowConfig)

	err = yaml.Unmarshal([]byte(data), conf)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal config: %w", err)
	}

	return conf, nil
}
