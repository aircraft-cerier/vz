package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
)

var (
	install    bool
	recovery   bool
	nbdURL     string
	cpu        uint
	mem        uint64
	macAddr    *vz.MACAddress
	gui        bool
	bundleName string
	ipsw       string
	vmShare    string
)

func init() {
	flag.BoolVar(&install, "install", false, "run command as install mode")
	flag.BoolVar(&recovery, "recovery", false, "boot VM into recovery mode")
	flag.StringVar(&nbdURL, "nbd-url", "", "nbd url (e.g. nbd+unix:///export?socket=nbd.sock)")
	flag.UintVar(&cpu, "cpu", 0, "CPU to use for VM, default is Total cores minus 1")
	flag.Uint64Var(&mem, "mem", 0, "Memory to use, default is 120gb")
	flag.StringVar(&bundleName, "bundle", "", "Name of vm bundle to start, defaults to VM")
	flag.BoolVar(&gui, "gui", false, "Whether to start a GUI for interacting,")
	flag.StringVar(&ipsw, "ipsw", "", "Name of ipsw to install")
	flag.StringVar(&vmShare, "vmshare", "", "Directory to mount to VM")
}

func main() {
	flag.Parse()
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if install {
		return installMacOS(ctx)
	}
	if recovery {
		return runRecoveryVM(ctx)
	}
	return runVM(ctx)
}

func runRecoveryVM(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	platformConfig, err := createMacPlatformConfiguration()
	if err != nil {
		return err
	}
	config, err := setupVMConfiguration(platformConfig)
	if err != nil {
		return err
	}
	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return err
	}

	if err := vm.Start(vz.WithStartUpFromMacOSRecovery(true)); err != nil {
		return err
	}

	errCh := make(chan error, 1)

	go func() {
		for {
			select {
			case newState := <-vm.StateChangedNotify():
				if newState == vz.VirtualMachineStateRunning {
					log.Println("start VM is running")
				}
				if newState == vz.VirtualMachineStateStopped || newState == vz.VirtualMachineStateStopping {
					log.Println("stopped state")
					errCh <- nil
					return
				}
			case err := <-errCh:
				errCh <- fmt.Errorf("failed to start vm: %w", err)
				return
			}
		}
	}()

	// it start listening to the NBD server, if any
	nbdAttachment := retrieveNetworkBlockDeviceStorageDeviceAttachment(config.StorageDevices())
	if nbdAttachment != nil {
		go func() {
			for {
				select {
				case err := <-nbdAttachment.DidEncounterError():
					log.Printf("NBD client has been encountered error: %v\n", err)
				case <-nbdAttachment.Connected():
					log.Println("NBD client connected with the server")
				}
			}
		}()
	}

	// cleanup is this function is useful when finished graphic application.
	cleanup := func() {
		for i := 1; vm.CanRequestStop(); i++ {
			result, err := vm.RequestStop()
			log.Printf("sent stop request(%d): %t, %v", i, result, err)
			time.Sleep(time.Second * 3)
			if i > 3 {
				log.Println("call stop")
				if err := vm.Stop(); err != nil {
					log.Println("stop with error", err)
					return
				}
				// if err := vm.Pause(); err != nil {
				// 	log.Println("pause with error", err)
				// 	return
				// }
				// if err := vm.SaveMachineStateToPath("savestate"); err != nil {
				// 	log.Println("save state with error", err)
				// }
			}
		}
		log.Println("finished cleanup")
	}

	vm.StartGraphicApplication(960, 600, vz.WithWindowTitle("macOS"), vz.WithController(true))

	cleanup()

	return <-errCh
}

func runVM(ctx context.Context) error {
	if gui {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}

	platformConfig, err := createMacPlatformConfiguration()
	if err != nil {
		return err
	}
	config, err := setupVMConfiguration(platformConfig)
	if err != nil {
		return err
	}
	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return err
	}

	if err := vm.Start(); err != nil {
		return err
	}

	errCh := make(chan error, 1)

	go func() {
		for {
			select {
			case newState := <-vm.StateChangedNotify():
				if newState == vz.VirtualMachineStateRunning {
					log.Println("start VM is running")
				}
				if newState == vz.VirtualMachineStateStopped || newState == vz.VirtualMachineStateStopping {
					log.Println("stopped state")
					errCh <- nil
					return
				}
			case err := <-errCh:
				errCh <- fmt.Errorf("failed to start vm: %w", err)
				return
			}
		}
	}()

	// it start listening to the NBD server, if any
	nbdAttachment := retrieveNetworkBlockDeviceStorageDeviceAttachment(config.StorageDevices())
	if nbdAttachment != nil {
		go func() {
			for {
				select {
				case err := <-nbdAttachment.DidEncounterError():
					log.Printf("NBD client has been encountered error: %v\n", err)
				case <-nbdAttachment.Connected():
					log.Println("NBD client connected with the server")
				}
			}
		}()
	}

	if !gui {
		for i := 1; i <= 15; i++ {
			vmIPAddr, err := getIPByMAC(macAddr)
			if err == nil {
				log.Println("VM's IP Address is: ", vmIPAddr)
				break
			}
			if i < 15 {
				time.Sleep(2 * time.Second)
			} else {
				log.Println("Failed to get IP Address of VM: %w after 30 seconds", "err", err)
				return err
			}
		}
	} else {
		log.Println("Starting with GUI")
		// cleanup is this function is useful when finished graphic application.
		cleanup := func() {
			for i := 1; vm.CanRequestStop(); i++ {
				result, err := vm.RequestStop()
				log.Printf("sent stop request(%d): %t, %v", i, result, err)
				time.Sleep(time.Second * 3)
				if i > 3 {
					log.Println("call stop")
					if err := vm.Stop(); err != nil {
						log.Println("stop with error", err)
						return
					}
					// if err := vm.Pause(); err != nil {
					// 	log.Println("pause with error", err)
					// 	return
					// }
					// if err := vm.SaveMachineStateToPath("savestate"); err != nil {
					// 	log.Println("save state with error", err)
					// }
				}
			}
			log.Println("finished cleanup")
		}

		vm.StartGraphicApplication(960, 600, vz.WithWindowTitle("macOS"), vz.WithController(true))

		cleanup()
	}

	return <-errCh
}

func computeCPUCount() uint {
	totalAvailableCPUs := runtime.NumCPU()
	virtualCPUCount := uint(totalAvailableCPUs - 1)
	if virtualCPUCount <= 1 {
		virtualCPUCount = 1
	}
	if cpu != 0 {
		virtualCPUCount = cpu
	}
	// TODO(codehex): use generics function when deprecated Go 1.17
	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	if virtualCPUCount > maxAllowed {
		virtualCPUCount = maxAllowed
	}
	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	if virtualCPUCount < minAllowed {
		virtualCPUCount = minAllowed
	}
	return virtualCPUCount
}

func computeMemorySize() uint64 {
	// We arbitrarily choose 4GB.
	memorySize := uint64(120 * 1024 * 1024 * 1024)
	if mem != 0 {
		memorySize = uint64(mem * 1024 * 1024 * 1024)
	}
	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	if memorySize > maxAllowed {
		memorySize = maxAllowed
	}
	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	if memorySize < minAllowed {
		memorySize = minAllowed
	}
	return memorySize
}

func createBlockDeviceConfiguration(diskPath string) (*vz.VirtioBlockDeviceConfiguration, error) {
	// create disk image with 256 GiB
	if err := vz.CreateDiskImage(diskPath, 160*1024*1024*1024); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create disk image: %w", err)
		}
	}

	attachment, err := vz.NewDiskImageStorageDeviceAttachment(
		diskPath,
		false,
	)
	if err != nil {
		return nil, err
	}
	return vz.NewVirtioBlockDeviceConfiguration(attachment)
}

func createNetworkBlockDeviceConfiguration(nbdURL string) (*vz.VirtioBlockDeviceConfiguration, error) {
	attachment, err := vz.NewNetworkBlockDeviceStorageDeviceAttachment(
		nbdURL,
		10*time.Second,
		false,
		vz.DiskSynchronizationModeFull,
	)
	if err != nil {
		return nil, err
	}
	return vz.NewVirtioBlockDeviceConfiguration(attachment)
}

func createGraphicsDeviceConfiguration() (*vz.MacGraphicsDeviceConfiguration, error) {
	graphicDeviceConfig, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	graphicsDisplayConfig, err := vz.NewMacGraphicsDisplayConfiguration(1920, 1200, 80)
	if err != nil {
		return nil, err
	}
	graphicDeviceConfig.SetDisplays(
		graphicsDisplayConfig,
	)
	return graphicDeviceConfig, nil
}

func createNetworkDeviceConfiguration() (*vz.VirtioNetworkDeviceConfiguration, error) {
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	return vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
}

func createKeyboardConfiguration() (vz.KeyboardConfiguration, error) {
	config, err := vz.NewMacKeyboardConfiguration()
	if err != nil {
		if errors.Is(err, vz.ErrUnsupportedOSVersion) {
			return vz.NewUSBKeyboardConfiguration()
		}
		return nil, err
	}
	return config, nil
}

func createAudioDeviceConfiguration() (*vz.VirtioSoundDeviceConfiguration, error) {
	audioConfig, err := vz.NewVirtioSoundDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create sound device configuration: %w", err)
	}
	inputStream, err := vz.NewVirtioSoundDeviceHostInputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create input stream configuration: %w", err)
	}
	outputStream, err := vz.NewVirtioSoundDeviceHostOutputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create output stream configuration: %w", err)
	}
	audioConfig.SetStreams(
		inputStream,
		outputStream,
	)
	return audioConfig, nil
}

func createMacPlatformConfiguration() (*vz.MacPlatformConfiguration, error) {
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(GetAuxiliaryStoragePath())
	if err != nil {
		return nil, fmt.Errorf("failed to create a new mac auxiliary storage: %w", err)
	}
	hardwareModel, err := vz.NewMacHardwareModelWithDataPath(
		GetHardwareModelPath(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new hardware model: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifierWithDataPath(
		GetMachineIdentifierPath(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new machine identifier: %w", err)
	}
	return vz.NewMacPlatformConfiguration(
		vz.WithMacAuxiliaryStorage(auxiliaryStorage),
		vz.WithMacHardwareModel(hardwareModel),
		vz.WithMacMachineIdentifier(machineIdentifier),
	)
}

func setupVMConfiguration(platformConfig vz.PlatformConfiguration) (*vz.VirtualMachineConfiguration, error) {
	bootloader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, err
	}

	config, err := vz.NewVirtualMachineConfiguration(
		bootloader,
		computeCPUCount(),
		computeMemorySize(),
	)
	if err != nil {
		return nil, err
	}
	config.SetPlatformVirtualMachineConfiguration(platformConfig)
	graphicsDeviceConfig, err := createGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create graphics device configuration: %w", err)
	}
	config.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{
		graphicsDeviceConfig,
	})
	blockDeviceConfig, err := createBlockDeviceConfiguration(GetDiskImagePath())
	if err != nil {
		return nil, fmt.Errorf("failed to create block device configuration: %w", err)
	}
	sdconfigs := []vz.StorageDeviceConfiguration{blockDeviceConfig}
	if nbdURL != "" {
		ndbConfig, err := createNetworkBlockDeviceConfiguration(nbdURL)
		if err != nil {
			return nil, fmt.Errorf("failed to create network block device configuration: %w", err)
		}
		sdconfigs = append(sdconfigs, ndbConfig)
	}
	config.SetStorageDevicesVirtualMachineConfiguration(sdconfigs)

	networkDeviceConfig, err := createNetworkDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create network device configuration: %w", err)
	}

	macAddr, err = vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to create random MAC Address: %w", err)
	}
	networkDeviceConfig.SetMACAddress(macAddr)

	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkDeviceConfig,
	})

	usbScreenPointingDevice, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create pointing device configuration: %w", err)
	}
	pointingDevices := []vz.PointingDeviceConfiguration{usbScreenPointingDevice}

	trackpad, err := vz.NewMacTrackpadConfiguration()
	if err == nil {
		pointingDevices = append(pointingDevices, trackpad)
	}
	config.SetPointingDevicesVirtualMachineConfiguration(pointingDevices)

	keyboardDeviceConfig, err := createKeyboardConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create keyboard device configuration: %w", err)
	}
	config.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{
		keyboardDeviceConfig,
	})

	audioDeviceConfig, err := createAudioDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create audio device configuration: %w", err)
	}
	config.SetAudioDevicesVirtualMachineConfiguration([]vz.AudioDeviceConfiguration{
		audioDeviceConfig,
	})

	if vmShare != "" {
		shareDir, err := vz.NewSharedDirectory("/Users/devicelab/vmshare", true)
		if err != nil {
			return nil, fmt.Errorf("failed to create directory share: %w", err)
		}
		singleDirShare, err := vz.NewSingleDirectoryShare(shareDir)
		if err != nil {
			return nil, fmt.Errorf("failed to create signle directory share: %w", err)
		}
		sharedDir, err := vz.NewVirtioFileSystemDeviceConfiguration("vmshare")
		if err != nil {
			return nil, err
		}
		sharedDir.SetDirectoryShare(singleDirShare)

		config.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{
			sharedDir,
		})
	}

	validated, err := config.Validate()
	if err != nil {
		return nil, fmt.Errorf("failed to validate configuration: %w", err)
	}
	if !validated {
		return nil, fmt.Errorf("invalid configuration")
	}

	// If you want to try this one, you need to comment out a few of configs.
	//
	// if _, err := config.ValidateSaveRestoreSupport(); err != nil {
	// 	return nil, fmt.Errorf("failed to validate save restore configuration: %w", err)
	// }

	return config, nil
}

func retrieveNetworkBlockDeviceStorageDeviceAttachment(storages []vz.StorageDeviceConfiguration) *vz.NetworkBlockDeviceStorageDeviceAttachment {
	for _, storage := range storages {
		attachment := storage.Attachment()
		if nbdAttachment, ok := attachment.(*vz.NetworkBlockDeviceStorageDeviceAttachment); ok {
			return nbdAttachment
		}
	}
	return nil
}

func getIPByMAC(mac *vz.MACAddress) (ipAddr string, err error) {
	// Normalize MAC to lowercase for comparison
	macAddr := strings.ToLower(mac.String())
	macRegex := regexp.MustCompile(`([0-9a-f]{1,2}[:-]){5}[0-9a-f]{1,2}`)
	for i := 1; i <= 15; i++ {
		out, err := exec.Command("arp", "-a").Output()
		if err != nil {
			return "", err
		}

		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			foundMAC := macRegex.FindString(strings.ToLower(line))
			if normalizeMAC(foundMAC) == normalizeMAC(macAddr) {
				// Try extracting the IP from parentheses
				re := regexp.MustCompile(`\(([^)]+)\)`)
				match := re.FindStringSubmatch(line)
				if len(match) > 1 {
					return match[1], nil
				}
			}
		}
		if i < 15 {
			time.Sleep(2 * time.Second)
		}
	}
	return "", fmt.Errorf("MAC address %s not found in ARP table", mac)
}

func normalizeMAC(mac string) string {
	parts := regexp.MustCompile(`[:-]`).Split(strings.ToLower(mac), -1)
	for i := range parts {
		parts[i] = fmt.Sprintf("%02x", parseHex(parts[i]))
	}
	return strings.Join(parts, ":")
}

func parseHex(s string) int {
	n, _ := strconv.ParseInt(s, 16, 0)
	return int(n)
}
