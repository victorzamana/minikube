/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/mcnerror"
	"github.com/golang/glog"
	"github.com/mitchellh/go-ps"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	cmdcfg "k8s.io/minikube/cmd/minikube/cmd/config"
	"k8s.io/minikube/pkg/minikube/cluster"
	pkg_config "k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/exit"
	"k8s.io/minikube/pkg/minikube/kubeconfig"
	"k8s.io/minikube/pkg/minikube/localpath"
	"k8s.io/minikube/pkg/minikube/machine"
	"k8s.io/minikube/pkg/minikube/out"
)

const (
	purge    = "purge"
	forcedelete = "forcedelete"
)

// deleteCmd represents the delete command
var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Deletes a local kubernetes cluster",
	Long: `Deletes a local kubernetes cluster. This command deletes the VM, and removes all
associated files.`,
	Run: runDelete,
}

func init() {
	deleteCmd.Flags().Bool(purge, false, "Set this flag to delete the '.minikube' folder from your user directory. This will prompt for confirmation.")
	deleteCmd.Flags().Bool(forcedelete, false, "Set this flag to delete all configurations and profiles")

	if err := viper.BindPFlags(deleteCmd.Flags()); err != nil {
		exit.WithError("unable to bind flags", err)
	}
}

// runDelete handles the executes the flow of "minikube delete"
func runDelete(cmd *cobra.Command, args []string) {
	if len(args) > 0 {
		exit.UsageT("Usage: minikube delete")
	}

	validProfiles, _, err := pkg_config.ListProfiles()

	// Check if the purge flag has been set but the force flag hasn't been set in case there are multiple profiles to delete.
	// If the following condition is not met, error out.
	glog.Infof("%v", viper.IsSet(forcedelete))
	glog.Infof("%v", viper.GetBool(forcedelete))
	if err == nil && viper.GetBool(purge) && len(validProfiles) > 1 && !viper.GetBool(forcedelete) {
		out.T(out.Embarrassed, "Multiple minikube profiles were found - ")
		for _, p := range validProfiles {
			out.T(out.Notice,"    - {{.profileName}}", out.V{"profileName":p.Name})
		}
		out.T(out.Notice, "Please use the {{.forceFlag}} to delete all of the configuration and the profiles.", out.V{"forceFlag":forcedelete})
		return
	}

	// Perform all the procedure for each of the profiles which are available.
	// For example, what if there are multiple profiles with multiple clusters running?
	for _, p := range validProfiles {
		deleteProfile(p.Name)
	}

	// If the purge flag is set, go ahead and delete the .minikube directory.
	if viper.GetBool(purge) {
		glog.Infof("Purging the '.minikube' directory located at %s", localpath.MiniPath())
		if err := os.RemoveAll(localpath.MiniPath()); err != nil {
			exit.WithError("unable to delete minikube config folder", err)
		}
		out.T(out.Crushed, "Successfully purged minikube directory located at - [{{.minikubeDirectory}}]", out.V{"minikubeDirectory":localpath.MiniPath()})
	}
}

func deleteProfile (profile string) {
	glog.Infof("Setting current profile to -- %v", profile)
	err := cmdcfg.Set(pkg_config.MachineProfile, profile)
	if err != nil {
		exit.WithError("Setting profile failed", err)
	}
	glog.Infof("Successfully set profile to -- %v", profile)

	api, err := machine.NewAPIClient()
	if err != nil {
		exit.WithError("Error getting client", err)
	}
	defer api.Close()

	cc, err := pkg_config.Load()
	if err != nil && !os.IsNotExist(err) {
		out.ErrT(out.Sad, "Error loading profile {{.name}}: {{.error}}", out.V{"name": profile, "error": err})
	}

	// In the case of "none", we want to uninstall Kubernetes as there is no VM to delete
	if err == nil && cc.MachineConfig.VMDriver == constants.DriverNone {
		uninstallKubernetes(api, cc.KubernetesConfig, viper.GetString(cmdcfg.Bootstrapper))
	}

	if err := killMountProcess(); err != nil {
		out.T(out.FailureType, "Failed to kill mount process: {{.error}}", out.V{"error": err})
	}

	if err = cluster.DeleteHost(api); err != nil {
		switch errors.Cause(err).(type) {
		case mcnerror.ErrHostDoesNotExist:
			out.T(out.Meh, `"{{.name}}" cluster does not exist. Proceeding ahead with cleanup.`, out.V{"name": profile})
		default:
			out.T(out.FailureType, "Failed to delete cluster: {{.error}}", out.V{"error": err})
			out.T(out.Notice, `You may need to manually remove the "{{.name}}" VM from your hypervisor`, out.V{"name": profile})
		}
	}

	// In case DeleteHost didn't complete the job.
	deleteProfileDirectory(profile)

	if err := pkg_config.DeleteProfile(profile); err != nil {
		if os.IsNotExist(err) {
			out.T(out.Meh, `"{{.name}}" profile does not exist`, out.V{"name": profile})
			os.Exit(0)
		}
		exit.WithError("Failed to remove profile", err)
	}
	out.T(out.Crushed, `The "{{.name}}" cluster has been deleted.`, out.V{"name": profile})

	machineName := pkg_config.GetMachineName()
	if err := kubeconfig.DeleteContext(constants.KubeconfigPath, machineName); err != nil {
		exit.WithError("update config", err)
	}

	if err := cmdcfg.Unset(pkg_config.MachineProfile); err != nil {
		exit.WithError("unset minikube profile", err)
	}
}

func uninstallKubernetes(api libmachine.API, kc pkg_config.KubernetesConfig, bsName string) {
	out.T(out.Resetting, "Uninstalling Kubernetes {{.kubernetes_version}} using {{.bootstrapper_name}} ...", out.V{"kubernetes_version": kc.KubernetesVersion, "bootstrapper_name": bsName})
	clusterBootstrapper, err := getClusterBootstrapper(api, bsName)
	if err != nil {
		out.ErrT(out.Empty, "Unable to get bootstrapper: {{.error}}", out.V{"error": err})
	} else if err = clusterBootstrapper.DeleteCluster(kc); err != nil {
		out.ErrT(out.Empty, "Failed to delete cluster: {{.error}}", out.V{"error": err})
	}
}

func deleteProfileDirectory(profile string) {
	machineDir := filepath.Join(localpath.MiniPath(), "machines", profile)
	if _, err := os.Stat(machineDir); err == nil {
		out.T(out.DeletingHost, `Removing {{.directory}} ...`, out.V{"directory": machineDir})
		err := os.RemoveAll(machineDir)
		if err != nil {
			exit.WithError("Unable to remove machine directory: %v", err)
		}
	}

	profileDir := filepath.Join(localpath.MiniPath(), "profiles", profile)
	if _, err := os.Stat(profileDir); err == nil {
		out.T(out.DeletingHost, `Removing {{.directory}} ...`, out.V{"directory": profileDir})
		err := os.RemoveAll(profileDir)
		if err != nil {
			exit.WithError("Unable to remove machine directory: %v", err)
		}
	}
}

// killMountProcess kills the mount process, if it is running
func killMountProcess() error {
	pidPath := filepath.Join(localpath.MiniPath(), constants.MountProcessFileName)
	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		return nil
	}

	glog.Infof("Found %s ...", pidPath)
	out, err := ioutil.ReadFile(pidPath)
	if err != nil {
		return errors.Wrap(err, "ReadFile")
	}
	glog.Infof("pidfile contents: %s", out)
	pid, err := strconv.Atoi(string(out))
	if err != nil {
		return errors.Wrap(err, "error parsing pid")
	}
	// os.FindProcess does not check if pid is running :(
	entry, err := ps.FindProcess(pid)
	if err != nil {
		return errors.Wrap(err, "ps.FindProcess")
	}
	if entry == nil {
		glog.Infof("Stale pid: %d", pid)
		if err := os.Remove(pidPath); err != nil {
			return errors.Wrap(err, "Removing stale pid")
		}
		return nil
	}

	// We found a process, but it still may not be ours.
	glog.Infof("Found process %d: %s", pid, entry.Executable())
	proc, err := os.FindProcess(pid)
	if err != nil {
		return errors.Wrap(err, "os.FindProcess")
	}

	glog.Infof("Killing pid %d ...", pid)
	if err := proc.Kill(); err != nil {
		glog.Infof("Kill failed with %v - removing probably stale pid...", err)
		if err := os.Remove(pidPath); err != nil {
			return errors.Wrap(err, "Removing likely stale unkillable pid")
		}
		return errors.Wrap(err, fmt.Sprintf("Kill(%d/%s)", pid, entry.Executable()))
	}
	return nil
}
