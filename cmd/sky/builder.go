package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io/ioutil"
	"os/exec"
	"path"
	"strings"
)

type builder struct {
	BuildConfig  buildConfig  `json:"Build"`
	DeployConfig deployConfig `json:"Deploy"`

	term        Terminal
	scm         Scm
	projectPath string
	pack        *build.Package
}

type buildConfig struct {
	Host       string
	User       string
	Jail       string
	CgoCFlags  string `json:"CGO_CFLAGS"`
	CgoLdFlags string `json:"CGO_LDFLAGS"`
	GoRoot     string
	GoPath     string

	AppRepo    string
	AppPath    string
	RepoType   string
	RepoBranch string

	UpdatePackages   bool
	BuildAllPackages bool
	RunTests         bool
	TestSkynet       bool

	PreBuildCommands  []string
	PostBuildCommands []string
}

type deployConfig struct {
	DeployPath string
	BinaryName string
}

var context = build.Default

func newBuilder(config string) *builder {
	if config == "" {
		config = "./build.cfg"
	}

	f, err := ioutil.ReadFile(config)

	if err != nil {
		panic("Failed to read: " + config)
	}

	b := new(builder)

	err = json.Unmarshal(f, b)

	if err != nil {
		panic("Failed to parse " + config + ": " + err.Error())
	}

	if isHostLocal(b.BuildConfig.Host) {
		b.term = new(LocalTerminal)
	} else {
		sshClient := new(SSHConn)
		b.term = sshClient
		sshClient.Connect(b.BuildConfig.Host, b.BuildConfig.User)
	}

	b.validatePackage()

	return b
}

func Build(config string) {
	b := newBuilder(config)
	b.performBuild()
	b.term.Close()
}

func Deploy(config string) {
	b := newBuilder(config)
	b.deploy([]string{"localhost"})
	b.term.Close()
}

func (b *builder) performBuild() {
	b.setupScm()

	if b.validateBuildEnvironment() {
		b.updateCode()

		b.term.SetEnv("GOPATH", b.goPath())
		b.term.SetEnv("GOROOT", b.BuildConfig.GoRoot)
		b.term.SetEnv("CGO_CFLAGS", b.BuildConfig.CgoCFlags)
		b.term.SetEnv("CGO_LDFLAGS", b.BuildConfig.CgoLdFlags)

		b.runCommands(b.BuildConfig.PreBuildCommands)

		b.updateDependencies()

		b.buildProject()

		if b.BuildConfig.RunTests {
			b.runTests()
		}

		b.runCommands(b.BuildConfig.PostBuildCommands)
	}
}

func (b *builder) validatePackage() {
	// Validate this package is a command
	var err error
	b.pack, err = context.ImportDir(".", 0)

	if err != nil {
		panic("Could not import package for validation")
	}

	if !b.pack.IsCommand() {
		panic("Package is not a command")
	}
}

// Ensure all directories exist
func (b *builder) validateBuildEnvironment() (valid bool) {
	var err error
	valid = true

	// Validate Jail exists
	_, err = b.term.Exec("ls " + b.BuildConfig.Jail)
	if err != nil {
		fmt.Println("Could not find Jail directory: " + err.Error())
		valid = false
	}

	// Validate GOROOT exists
	_, err = b.term.Exec("ls " + b.BuildConfig.GoRoot)
	if err != nil {
		fmt.Println("Could not find GOROOT directory: " + err.Error())
		valid = false
	}

	// Validate Go Binary exists
	_, err = b.term.Exec("ls " + b.BuildConfig.GoRoot + "/bin/go")
	if err != nil {
		fmt.Println("Could not find Go binary: " + err.Error())
		valid = false
	}

	// Validate Git exists
	_, err = b.term.Exec("which " + b.scm.BinaryName())
	if err != nil {
		fmt.Println("Could not find " + b.BuildConfig.RepoType + " binary: " + err.Error())
		valid = false
	}

	return
}

// Checkout project from repository
func (b *builder) updateCode() {
	p, err := b.scm.ImportPathFromRepo(b.BuildConfig.AppRepo)
	b.projectPath = path.Join(b.BuildConfig.Jail, "src", p)

	if err != nil {
		panic(err.Error())
	}

	out, err := b.term.Exec("ls " + b.projectPath)

	if err != nil {
		fmt.Println("Creating project directories")
		out, err = b.term.Exec("mkdir -p " + b.projectPath)

		if err != nil {
			panic("Could not create project directories")
		}

		fmt.Println(string(out))
	}

	// Fetch code base
	b.scm.SetTerminal(b.term)
	b.scm.Checkout(b.BuildConfig.AppRepo, b.BuildConfig.RepoBranch, b.projectPath)
}

func (b *builder) setupScm() {
	switch b.BuildConfig.RepoType {
	case "git":
		b.scm = new(GitScm)

	default:
		panic("unkown RepoType")
	}
}

func (b *builder) updateDependencies() {
	b.getPackageDependencies(path.Join(b.projectPath, b.BuildConfig.AppPath))
}

func (b *builder) buildProject() {
	p := path.Join(b.projectPath, b.BuildConfig.AppPath)
	flags := "-v"

	if b.BuildConfig.BuildAllPackages {
		flags = flags + " -a"
	}

	fmt.Println("Building packages")
	out, err := b.term.ExecPath("go install "+flags, p)
	fmt.Println(string(out))

	if err != nil {
		panic("Failed build: " + err.Error())
	}
}

func (b *builder) runTests() {
	p := path.Join(b.projectPath, b.BuildConfig.AppPath)

	fmt.Println("Testing packages")
	out, err := b.term.ExecPath("go test", p)
	fmt.Println(string(out))

	if err != nil {
		panic("Failed tests: " + err.Error())
	}

	if b.BuildConfig.TestSkynet {
		b.testSkynet()
	}
}

func (b *builder) testSkynet() {
	fmt.Println("Testing Skynet")
	p := path.Join(b.BuildConfig.Jail, "src/github.com/skynetservices/skynet2")

	b.getPackageDependencies(p)

	out, err := b.term.ExecPath("go test ./...", p)
	fmt.Println(string(out))

	if err != nil {
		panic("Failed tests: " + err.Error())
	}
}

func (b *builder) getPackageDependencies(p string) {
	flags := []string{"-d"}

	if b.BuildConfig.UpdatePackages {
		flags = append(flags, "-u")
	}

	fmt.Println("Fetching dependencies")
	out, err := b.term.ExecPath("go get "+strings.Join(flags, " ")+" ./...", p)
	fmt.Println(string(out))

	if err != nil {
		panic("Failed to fetch dependencies\n" + err.Error())
	}
}

func (b *builder) runCommands(cmds []string) {
	for _, cmd := range cmds {
		out, err := b.term.Exec(cmd)
		fmt.Println(string(out))

		if err != nil {
			panic("Failed to execute dependent command: " + cmd + "\n" + err.Error())
		}
	}
}

func (b *builder) goPath() string {
	if b.BuildConfig.GoPath != "" {
		return b.BuildConfig.Jail + ":" + b.BuildConfig.GoPath
	}

	return b.BuildConfig.Jail
}

func (b *builder) deploy(hosts []string) {
	for _, host := range hosts {
		var out []byte
		var err error

		// TODO: if build and deploy boxes are remote, need to scp from server to server
		if isHostLocal(host) && isHostLocal(b.BuildConfig.Host) {
			fmt.Println("Copying local binary")
			command := exec.Command("cp", path.Join(b.BuildConfig.Jail, "bin", path.Base(b.BuildConfig.AppPath)), path.Join(b.DeployConfig.DeployPath, b.DeployConfig.BinaryName))
			out, err = command.CombinedOutput()
		} else if isHostLocal(host) && !isHostLocal(b.BuildConfig.Host) {
			// Deploying locally but build is remote
			fmt.Println("Copying binary from build machine")
			host, port := splitHostPort(b.BuildConfig.Host)

			command := exec.Command("scp", "-P", port, b.BuildConfig.User+"@"+host+":"+path.Join(b.BuildConfig.Jail, "bin", path.Base(b.BuildConfig.AppPath)),
				path.Join(b.DeployConfig.DeployPath, b.DeployConfig.BinaryName))
			out, err = command.CombinedOutput()
		}

		fmt.Println(string(out))

		if err != nil {
			panic("Failed to deploy: " + err.Error())
		}
	}
}

func splitHostPort(host string) (string, string) {
	parts := strings.Split(host, ":")

	if len(parts) < 2 {
		return parts[0], "22"
	} else {
		return parts[0], parts[1]
	}
}

func isHostLocal(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "" {
		return true
	}

	return false
}
