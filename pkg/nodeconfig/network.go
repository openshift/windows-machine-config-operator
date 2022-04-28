package nodeconfig

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
)

// numberOfPolicies are the required number of policies in CNI config
const numberOfPolicies = 3

// cniConf contains the structure of the CNI template
type cniConf struct {
	CNIVersion   string       `json:"cniVersion"`
	ApiVersion   int          `json:"apiVersion"`
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Capabilities capabilities `json:"capabilities"`
	IPAM         ipam         `json:"ipam"`
	Policies     policies     `json:"policies"`
}

// nested structs required for creating cniConf struct
type capabilities struct {
	PortMappings bool `json:"portMappings"`
	DNS          bool `json:"dns"`
}

type ipam struct {
	Type   string `json:"type"`
	Subnet string `json:"subnet"`
}

type policies []struct {
	Name  string `json:"name"`
	Value value  `json:"value"`
}

type value struct {
	Type     string   `json:"Type"`
	Settings settings `json:"Settings,omitempty"`
}

type settings struct {
	ExceptionList     []string `json:"ExceptionList,omitempty"`
	DestinationPrefix string   `json:"DestinationPrefix,omitempty"`
	NeedEncap         bool     `json:"NeedEncap"`
	ProviderAddress   string   `json:"ProviderAddress,omitempty"`
}

// network struct contains the node network information
type network struct {
	// hostSubnet holds the node host subnet value
	hostSubnet string
	log        logr.Logger
}

// newNetwork returns a pointer to the network struct
func newNetwork(logger logr.Logger) *network {
	return &network{log: logger}
}

// setHostSubnet sets the value for hostSubnet field in the network struct
func (nw *network) setHostSubnet(hostSubnet string) error {
	if hostSubnet == "" || cluster.ValidateCIDR(hostSubnet) != nil {
		return errors.Errorf("error receiving valid value for node hostSubnet")
	}
	nw.hostSubnet = hostSubnet
	return nil
}

// cleanupTempConfig cleans up the temporary CNI directory and config file created
func (nw *network) cleanupTempConfig(configFile string) error {
	err := os.RemoveAll(configFile)
	if err != nil {
		nw.log.Error(err, "couldn't delete temp CNI config file", "configFile", configFile)
	}
	return nil
}

// populateCniConfig populates the CNI config template with the values for
// serviceCIDR and IP address of the Windows VM. It creates a new file in
// temp directory to store the modified template
func (nw *network) populateCniConfig(serviceCIDR string, ipAddress string, templatePath string) (string, error) {
	if nw.hostSubnet == "" {
		return "", errors.New("can't populate CNI config with empty hostSubnet")
	}

	cniConfTemplate, err := ioutil.ReadFile(templatePath)
	if err != nil {
		return "", errors.Wrapf(err, "error reading CNI config template from %s", templatePath)
	}

	cniCfg := cniConf{}
	if err = json.Unmarshal(cniConfTemplate, &cniCfg); err != nil {
		return "", errors.Wrap(err, "error converting CNI template into cniCfg struct")
	}

	if err = populateCfgPolicies(&cniCfg.Policies, serviceCIDR, ipAddress); err != nil {
		return "", errors.Wrap(err, "error populating config policies in cniConf struct")
	}

	cniCfg.IPAM.Subnet = nw.hostSubnet

	// retrieve the json file from the modified struct
	cniCfgBuf, err := json.Marshal(&cniCfg)
	if err != nil {
		return "", errors.Wrap(err, "can't retrieve cniConf JSON using modified struct")
	}

	// Create a temp file to hold the cniCfg
	tmpCniDir, err := ioutil.TempDir("", "cni")
	if err != nil {
		return "", errors.Wrap(err, "error creating Local temp CNI directory")
	}
	cniConfigPath, err := os.Create(filepath.Join(tmpCniDir, "cni.conf"))
	if err != nil {
		return "", errors.Wrap(err, "error creating local cni.conf file")
	}
	defer cniConfigPath.Close()

	if _, err = cniConfigPath.Write(cniCfgBuf); err != nil {
		return "", errors.Wrapf(err, "can't write JSON CNI config file to %s", cniConfigPath.Name())
	}
	if cniConfigPath.Name() == "" {
		return "", errors.Errorf("couldn't retrieve CNI config file %s", cniConfigPath.Name())
	}
	return cniConfigPath.Name(), nil
}

// populateCfgPolicies populates the policies in cniConf struct with serviceCIDR and
// the Windows host IP address information
func populateCfgPolicies(cniCfgPolicies *policies, serviceCIDR string, ipAddress string) error {
	if len(*cniCfgPolicies) < numberOfPolicies {
		return errors.Errorf("number of policies cannot be less than %d", numberOfPolicies)
	}
	if len((*cniCfgPolicies)[0].Value.Settings.ExceptionList) == 0 ||
		(*cniCfgPolicies)[1].Value.Settings.DestinationPrefix == "" ||
		(*cniCfgPolicies)[2].Value.Settings.ProviderAddress == "" {
		return errors.Errorf("invalid policy fields in cniConf struct")
	}
	(*cniCfgPolicies)[0].Value.Settings.ExceptionList[0] = serviceCIDR
	(*cniCfgPolicies)[1].Value.Settings.DestinationPrefix = serviceCIDR
	(*cniCfgPolicies)[2].Value.Settings.ProviderAddress = ipAddress
	return nil
}
