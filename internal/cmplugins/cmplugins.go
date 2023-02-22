package cmplugins

import (
	"fmt"
	"log"
	"os"

	"github.com/braintree/heckler/internal/cmplugins/tbstop"
)

type ChangeManagementHooksConfig struct {
	DeploymentMoratorium string `yaml:"deployment_moratorium"`
}

type DeploymentMoratorium func() (bool, error)

type ChangeManagementHooks struct {
	IsDeploymentMoratorium DeploymentMoratorium
}

// GetChangeManagementHooks initializes and registers change management hooks specified in ChangeManagementHooksConfig
// if there is no a hook configuration, stub function that does nothing is registered instead
func GetChangeManagementHooks(conf ChangeManagementHooksConfig) (*ChangeManagementHooks, error) {
	logger := log.New(os.Stdout, "[cmplugins] ", log.Lshortfile)

	hooks := new(ChangeManagementHooks)

	switch conf.DeploymentMoratorium {
	case "tbstop":
		tbstop, err := tbstop.InitTBStop()
		if err != nil {
			return nil, fmt.Errorf("initializing tbstop hook: %w", err)
		}
		hooks.IsDeploymentMoratorium = tbstop.IsDeploymentMoratorium
		logger.Printf("tbstop DeploymentMoratorium hook is registered")
	default:
		logger.Printf("DeploymentMoratorium hook is not configured")
		hooks.IsDeploymentMoratorium = func() (bool, error) { return false, nil }
	}

	return hooks, nil
}
