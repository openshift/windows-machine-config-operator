package windows

import (
	"errors"
	"fmt"
	"strings"
)

// HostsFilePath is the relative path to the hosts file in Windows OS
const HostsFilePath = "$env:windir\\System32\\drivers\\etc\\hosts"

func (vm *windows) addHostsEntry(ipAddress, hostname, comment string) error {
	// validations
	if ipAddress == "" {
		return errors.New("ipAddress cannot be empty")
	}
	if hostname == "" {
		return errors.New("hostname cannot be empty")
	}
	// check comment
	if comment != "" && !strings.HasPrefix(comment, "#") {
		// format
		comment = fmt.Sprintf("# %s", comment)
	}
	// build entry
	entry := fmt.Sprintf("%s  %s  %s", ipAddress, hostname, comment)
	// log
	vm.log.Info("adding entry to hosts file", "entry", entry)
	// single-quote and surround entry with a new line (`n)
	entry = fmt.Sprintf("`n'%s'`n", entry)
	// append entry to hosts file
	_, err := vm.Run("Add-Content "+
		"-Path "+HostsFilePath+" "+
		"-Value "+entry+" "+
		"-Force", true)
	if err != nil {
		return err
	}
	// return no error
	return nil
}
