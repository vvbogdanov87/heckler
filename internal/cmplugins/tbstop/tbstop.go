package tbstop

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Response from tbstop api
type TBResponse struct {
	Data []TBData `json:"data"`
}

type TBData struct {
	ChangestopStatus string `json:"changestop_status"`
}

type TBStop struct {
	Config *TBStopConfig
}

type TBStopConfig struct {
	URL string `yaml:"url"`
}

// IsDeploymentMoratorium gets deployment moratorium status
func (t *TBStop) IsDeploymentMoratorium() (bool, error) {
	httpCLient := &http.Client{
		Timeout: 30 * time.Second,
	}

	request, err := http.NewRequest("GET", t.Config.URL, nil)
	if err != nil {
		return false, fmt.Errorf("making a request object: %w", err)
	}

	resp, err := httpCLient.Do(request)
	if err != nil {
		return false, fmt.Errorf("doing a request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("get not 2xx response code: %d: %w", resp.StatusCode, err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("reading response body: %w", err)
	}

	tbstop := new(TBResponse)
	err = json.Unmarshal(body, tbstop)
	if err != nil {
		return false, fmt.Errorf("unmarshalling body: %w", err)
	}

	return tbstop.Data[0].ChangestopStatus != "Inactive", nil
}

// InitTBStop initializes and returns TBStop object
func InitTBStop() (*TBStop, error) {
	conf, err := getTBStopConfig()
	if err != nil {
		return nil, fmt.Errorf("getting tbstop config: %w", err)
	}

	tbstop := new(TBStop)
	tbstop.Config = conf

	return tbstop, nil
}

// getTBStopConfig reads tbstop configuration
func getTBStopConfig() (*TBStopConfig, error) {
	var tbstopConfPath string
	if _, err := os.Stat("/etc/hecklerd/tbstop_conf.yaml"); err == nil {
		tbstopConfPath = "/etc/hecklerd/tbstop_conf.yaml"
	} else if _, err := os.Stat("tbstop_conf.yaml"); err == nil {
		tbstopConfPath = "tbstop_conf.yaml"
	} else {
		return nil, fmt.Errorf("unable to load tbstop_conf.yaml from /etc/hecklerd or `.`: %w", err)
	}

	file, err := os.Open(tbstopConfPath)
	if err != nil {
		return nil, fmt.Errorf("unable to open tbstop_conf.yaml: %w", err)
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read tbstop_conf.yaml: %w", err)
	}

	conf := new(TBStopConfig)

	err = yaml.Unmarshal([]byte(data), conf)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal tbstop_conf.yaml: %w", err)
	}

	return conf, nil
}
