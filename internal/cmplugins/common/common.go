package common

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
)

func ReadConfig(fileName string) ([]byte, error) {
	var confPath string

	etcPath := path.Join("/etc/hecklerd", fileName)
	if _, err := os.Stat(etcPath); err == nil {
		confPath = etcPath
	} else if _, err := os.Stat("tbstop_conf.yaml"); err == nil {
		confPath = fileName
	} else {
		return nil, fmt.Errorf("unable to load %s from /etc/hecklerd or `.`: %w", fileName, err)
	}

	file, err := os.Open(confPath)
	if err != nil {
		return nil, fmt.Errorf("unable to open %s: %w", confPath, err)
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", confPath, err)
	}

	return data, nil
}
