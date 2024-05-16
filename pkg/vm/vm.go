package vm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"gitlab.com/bootc-org/podman-bootc/pkg/bootc"
	"gitlab.com/bootc-org/podman-bootc/pkg/config"
	"gitlab.com/bootc-org/podman-bootc/pkg/user"
	"gitlab.com/bootc-org/podman-bootc/pkg/utils"

	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

var ErrVMInUse = errors.New("VM already in use")

// GetVMCachePath returns the path to the VM cache directory
func GetVMCachePath(imageId string, user user.User) (longID string, path string, err error) {
	fullImageId, err := utils.FullImageIdFromPartial(imageId, user)
	if err != nil {
		return "", "", fmt.Errorf("error getting full image ID: %w", err)
	}

	return fullImageId, filepath.Join(user.CacheDir(), fullImageId), nil
}

type NewVMParameters struct {
	ImageID     string
	User        user.User //user who is running the podman bootc command
	LibvirtUri  string    //linux only
	CacheConfig config.CacheConfig //existing cache config
}

type RunVMParameters struct {
	VMUser        string //user to use when connecting to the VM
	CloudInitDir  string
	NoCredentials bool
	CloudInitData bool
	SSHIdentity   string
	SSHPort       int
	Cmd           []string
	RemoveVm      bool
	Background    bool
}

type BootcVM interface {
	Run(RunVMParameters) error
	Delete() error
	IsRunning() (bool, error)
	WaitForSSHToBeReady() error
	RunSSH(port int, identity string, cmd []string) error
	DeleteFromCache() error
	Exists() (bool, error)
	CloseConnection()
	PrintConsole() error
}

type BootcVMCommon struct {
	vmName        string
	cacheDir      string
	diskImagePath string
	vmUsername    string
	user          user.User
	sshIdentity   string
	sshPort       int
	removeVm      bool
	background    bool
	cmd           []string
	pidFile       string
	imageID       string
	imageDigest   string
	noCredentials bool
	hasCloudInit  bool
	cloudInitDir  string
	cloudInitArgs string
	bootcDisk     bootc.BootcDisk
	cacheConfig   config.CacheConfig
}

func (v *BootcVMCommon) SetUser(user string) error {
	if user == "" {
		return fmt.Errorf("user is required")
	}

	v.vmUsername = user
	return nil
}

func (v *BootcVMCommon) WaitForSSHToBeReady() error {
	timeout := 1 * time.Minute
	elapsed := 0 * time.Millisecond
	interval := 500 * time.Millisecond

	key, err := os.ReadFile(v.sshIdentity)
	if err != nil {
		return fmt.Errorf("failed to read private key file: %s\n", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %s\n", err)
	}

	config := &ssh.ClientConfig{
		User: v.vmUsername,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         1 * time.Second,
	}

	for elapsed < timeout {
		client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", "localhost", v.sshPort), config)
		if err != nil {
			logrus.Debugf("failed to connect to SSH server: %s\n", err)
			time.Sleep(interval)
			elapsed += interval
		} else {
			client.Close()
			return nil
		}
	}

	return fmt.Errorf("SSH did not become ready in %s seconds", timeout)
}

// RunSSH runs a command over ssh or starts an interactive ssh connection if no command is provided
func (v *BootcVMCommon) RunSSH(sshPort int, sshIdentity string, inputArgs []string) error {
	v.sshPort = sshPort
	v.sshIdentity = sshIdentity

	sshDestination := v.vmUsername + "@localhost"
	port := strconv.Itoa(v.sshPort)

	args := []string{"-i", v.sshIdentity, "-p", port, sshDestination,
		"-o", "IdentitiesOnly=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "StrictHostKeyChecking=no",
		"-o", "LogLevel=ERROR",
		"-o", "SetEnv=LC_ALL=",
		"-o", "UserKnownHostsFile=/dev/null"}
	if len(inputArgs) > 0 {
		args = append(args, inputArgs...)
	} else {
		fmt.Printf("Connecting to vm %s. To close connection, use `~.` or `exit`\n", v.imageID)
	}

	cmd := exec.Command("ssh", args...)

	logrus.Debugf("Running ssh command: %s", cmd.String())

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

// Delete removes the VM disk image and the VM configuration from the podman-bootc cache
func (v *BootcVMCommon) DeleteFromCache() error {
	return os.RemoveAll(v.cacheDir)
}

func (b *BootcVMCommon) oemString() (string, error) {
	tmpFilesCmd, err := b.tmpFileInjectSshKeyEnc()
	if err != nil {
		return "", err
	}
	oemString := fmt.Sprintf("type=11,value=io.systemd.credential.binary:tmpfiles.extra=%s", tmpFilesCmd)
	return oemString, nil
}

func (b *BootcVMCommon) tmpFileInjectSshKeyEnc() (string, error) {
	pubKey, err := os.ReadFile(b.sshIdentity + ".pub")
	if err != nil {
		return "", err
	}
	pubKeyEnc := base64.StdEncoding.EncodeToString(pubKey)

	userHomeDir := "/root"
	if b.vmUsername != "root" {
		userHomeDir = filepath.Join("/home", b.vmUsername)
	}

	tmpFileCmd := fmt.Sprintf("d %[1]s/.ssh 0750 %[2]s %[2]s -\nf+~ %[1]s/.ssh/authorized_keys 700 %[2]s %[2]s - %[3]s", userHomeDir, b.vmUsername, pubKeyEnc)

	tmpFileCmdEnc := base64.StdEncoding.EncodeToString([]byte(tmpFileCmd))
	return tmpFileCmdEnc, nil
}
