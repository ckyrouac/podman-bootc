package cmd

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gitlab.com/bootc-org/podman-bootc/pkg/config"
	"gitlab.com/bootc-org/podman-bootc/pkg/user"
	"gitlab.com/bootc-org/podman-bootc/pkg/vm"

	"github.com/containers/common/pkg/report"
	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// listCmd represents the hello command
var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed OS Containers",
	Long:  "List installed OS Containers",
	RunE:  doList,
}

func init() {
	RootCmd.AddCommand(listCmd)
}

func doList(_ *cobra.Command, _ []string) error {
	hdrs := report.Headers(config.CacheConfig{}, map[string]string{
		"RepoTag":  "Repo",
		"DiskSize": "Size",
	})

	rpt := report.New(os.Stdout, "list")
	defer rpt.Flush()

	rpt, err := rpt.Parse(
		report.OriginPodman,
		"{{range . }}{{.Id}}\t{{.RepoTag}}\t{{.DiskSize}}\t{{.Created}}\t{{.Running}}\t{{.SshPort}}\n{{end -}}")

	if err != nil {
		return err
	}

	if err := rpt.Execute(hdrs); err != nil {
		return err
	}

	user, err := user.NewUser()
	if err != nil {
		return err
	}

	vmList, err := CollectVmList(user, config.LibvirtUri)
	if err != nil {
		return err
	}

	return rpt.Execute(vmList)
}

func CollectVmList(user user.User, libvirtUri string) (vmList []config.CacheConfig, err error) {
	files, err := os.ReadDir(user.CacheDir())
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		if f.IsDir() {
			cfg, err := getVMInfo(user, libvirtUri, f.Name())
			if err != nil {
				logrus.Warningf("skipping vm %s reason: %v", f.Name(), err)
				continue
			}

			vmList = append(vmList, cfg)
		}
	}
	return vmList, nil
}

// getVMInfo loads the cached VM information from the config file
// and checks if the VM is running
func getVMInfo(user user.User, libvirtUri string, imageId string) (config.CacheConfig, error) {
	cacheConfigManager, err := config.NewCacheConfigManager(user.CacheDir(), imageId)
	if err != nil {
		return config.CacheConfig{}, err
	}
	cacheConfig, err := cacheConfigManager.Read()
	if err != nil {
		return cacheConfig, err
	}

	cacheConfig, err = formatConfigValues(cacheConfig)
	if err != nil {
		return cacheConfig, err
	}

	bootcVM, err := vm.NewVM(vm.NewVMParameters{
		ImageID:    imageId,
		User:       user,
		LibvirtUri: libvirtUri,
		CacheConfig: cacheConfig,
	})

	if err != nil {
		return cacheConfig, err
	}

	isRunning, err := bootcVM.IsRunning()
	if err != nil {
		return cacheConfig, err
	}
	cacheConfig.Running = isRunning

	defer func() {
		bootcVM.CloseConnection()
	}()

	return cacheConfig, nil
}

//format the config values for display
func formatConfigValues(cacheConfig config.CacheConfig) (config.CacheConfig, error) {
	if cacheConfig.Id != "" {
		cacheConfig.Id = cacheConfig.Id[:12]
	}

	if cacheConfig.Created != "" {
		createdTime, err := time.Parse(time.RFC3339, cacheConfig.Created)
		if err != nil {
			return cacheConfig, fmt.Errorf("error parsing created time: %w", err)
		}
		cacheConfig.Created = units.HumanDuration(time.Since(createdTime)) + " ago"
	}

	if cacheConfig.DiskSize != "" {
		diskSizeFloat, err := strconv.ParseFloat(cacheConfig.DiskSize, 64)
		if err != nil {
			return cacheConfig, fmt.Errorf("error parsing disk size: %w", err)
		}
		cacheConfig.DiskSize = units.HumanSizeWithPrecision(diskSizeFloat, 3)
	}

	return cacheConfig, nil
}
