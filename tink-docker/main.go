package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type tinkConfig struct {
	registry         string
	registryUsername string
	registryPassword string
	baseURL          string
	tinkerbell       string

	// TODO add others
}

func main() {
	fmt.Println("Starting Tink-Docker")
	go rebootWatch()

	// Parse the cmdline in order to find the urls for the repository and path to the cert
	content, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		panic(err)
	}
	cmdLines := strings.Split(string(content), " ")
	cfg := parseCmdLine(cmdLines)

	path := fmt.Sprintf("/etc/docker/certs.d/%s/", cfg.registry)

	// Create the directory
	err = os.MkdirAll(path, os.ModeDir)
	if err != nil {
		panic(err)
	}
	// Download the configuration
	fmt.Printf("Downloading the %s registry CA certificate\n", cfg.registry)
	err = downloadFile(path+"ca.crt", cfg.baseURL+"/ca.pem")
	if err != nil {
		panic(err)
	}

	// Install the docker plugins
	fmt.Println("Installing the Docker Engine plugins")
	err = installDockerPlugins(
		cfg.registry,
		cfg.registryUsername,
		cfg.registryPassword)
	if err != nil {
		panic(err)
	}

	// Build the command, and execute
	fmt.Println("Starting the Docker Engine")
	cmd := exec.Command(
		"/usr/local/bin/docker-init",
		"-s",
		"/usr/local/bin/dockerd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
}

func installDockerPlugins(registry string, registryUsername string, registryPassword string) error {
	// execute dockerd in background.
	// NB to install the plugins we need a running dockerd. we temporarily execute
	//    one in background to install the plugins.
	dockerHost := "unix:///dockerd.install.sock"
	dockerConfigPath := "/dockerd.install.json"
	err := ioutil.WriteFile(dockerConfigPath, []byte("{}"), 0644)
	if err != nil {
		return err
	}
	fmt.Println("Starting up the temporary docker daemon")
	cmd := exec.Command(
		"/usr/local/bin/dockerd",
		"--config-file",
		dockerConfigPath,
		"--host",
		dockerHost)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		return err
	}

	// wait for dockerd to be ready.
	err = waitForDockerDaemon(dockerHost)
	if err != nil {
		return err
	}

	// login into the registry.
	fmt.Printf("Logging in the %s registry\n", registry)
	err = dockerLogin(dockerHost, registry, registryUsername, registryPassword)
	if err != nil {
		return err
	}

	// install the loki log driver plugin.
	fmt.Println("Installing the loki log driver plugin")
	err = installDockerPlugin(
		dockerHost,
		"loki",
		fmt.Sprintf("%s/grafana/loki-docker-driver:2.3.0", registry),
		"LOG_LEVEL=info")
	if err != nil {
		return err
	}

	// TODO how to properly wait?
	fmt.Println("Waiting for the plugins to be ready")
	time.Sleep(15 * time.Second)

	// shutdown dockerd.
	fmt.Println("Shutting down the temporary docker daemon")
	cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	if err != nil {
		return err
	}

	os.Remove(dockerConfigPath)

	// TODO how to properly wait?
	//      in theory, neither containerd nor the plugins are shutdown by docker, so this might be moot.
	fmt.Println("Waiting for the containerd and docker plugins to shutdown")
	time.Sleep(15 * time.Second)

	return nil
}

func installDockerPlugin(dockerHost string, alias string, image string, arguments ...string) error {
	cmd := exec.Command(
		"/usr/local/bin/docker",
		append(
			[]string{
				"--host",
				dockerHost,
				"plugin",
				"install",
				"--grant-all-permissions",
				"--alias",
				alias,
				image,
			},
			arguments...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dockerLogin(dockerHost string, registry string, username string, password string) error {
	cmd := exec.Command(
		"/usr/local/bin/docker",
		"--host",
		dockerHost,
		"login",
		"--username",
		username,
		"--password-stdin",
		registry)
	cmd.Stdin = strings.NewReader(password)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForDockerDaemon(dockerHost string) error {
	for {
		cmd := exec.Command(
			"/usr/local/bin/docker",
			"--host",
			dockerHost,
			"info",
			"--format",
			"{{.ServerVersion}}")
		err := cmd.Run()
		if err == nil {
			return nil
		}
		if _, ok := err.(*exec.ExitError); ok {
			time.Sleep(1 * time.Second)
		} else {
			return err
		}
	}
}

// parseCmdLine will parse the command line.
func parseCmdLine(cmdLines []string) (cfg tinkConfig) {
	for i := range cmdLines {
		cmdLine := strings.Split(cmdLines[i], "=")
		if len(cmdLine) == 0 {
			continue
		}

		switch cmd := cmdLine[0]; cmd {
		// Find Registry configuration
		case "docker_registry":
			cfg.registry = cmdLine[1]
		case "registry_username":
			cfg.registryUsername = cmdLine[1]
		case "registry_password":
			cfg.registryPassword = cmdLine[1]
		case "packet_base_url":
			cfg.baseURL = cmdLine[1]
		case "tinkerbell":
			cfg.tinkerbell = cmdLine[1]
		}
	}
	return cfg
}

// downloadFile will download a url to a local file. It's efficient because it will
// write as it downloads and not load the whole file into memory.
func downloadFile(filepath string, url string) error {
	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}

func rebootWatch() {
	fmt.Println("Starting Reboot Watcher")

	// Forever loop
	for {
		if fileExists("/worker/reboot") {
			cmd := exec.Command("/sbin/reboot")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				panic(err)
			}
			break
		}
		// Wait one second before looking for file
		time.Sleep(time.Second)
	}
	fmt.Println("Rebooting")
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
