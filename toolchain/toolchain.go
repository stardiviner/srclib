package toolchain

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fsouza/go-dockerclient"
)

// srclibtoolchain represents a toolchain's Srclibtoolchain file.
type srclibtoolchain struct {
	// TODO(sqs): Right now, we just care about the existence of this file. When
	// we actually want to parse its fields, add them here.
}

// Info describes a toolchain.
type Info struct {
	// Path is the toolchain's path (not a directory path) underneath the
	// SRCLIBPATH. It consists of the URI of this repository's toolchain plus
	// its subdirectory path within the repository. E.g., "github.com/foo/bar"
	// for a toolchain defined in the root directory of that repository.
	Path string

	// Dir is the filesystem directory that defines this toolchain.
	Dir string

	// SrclibtoolchainFile is the path to the Srclibtoolchain file, relative to
	// Dir.
	SrclibtoolchainFile string

	// Program is the path to the executable program (relative to Dir) to run to
	// invoke this toolchain, for the program execution method.
	Program string `json:",omitempty"`

	// Dockerfile is the path to the Dockerfile (relative to Dir) that defines
	// the image to build and run to invoke this toolchain, for the Docker
	// container execution method.
	Dockerfile string `json:",omitempty"`
}

// Tools lists the tools that this toolchain implements (as subcommands).
func (t *Info) Tools() ([]*ToolInfo, error) {
	data, err := ioutil.ReadFile(filepath.Join(t.Dir, t.SrclibtoolchainFile))
	if err != nil {
		return nil, err
	}

	var tools []*ToolInfo
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("TOOL ")) {
			subcmd := string(bytes.TrimSpace(line[len("TOOL "):]))
			// TODO(sqs): assumes that tool subcmd == tool op, which is not true
			// in general.
			tools = append(tools, &ToolInfo{
				Toolchain: t,
				Subcmd:    subcmd,
				Op:        subcmd,
			})
		}
	}
	return tools, nil
}

// A Mode value is a set of flags (or 0) that control how toolchains are used.
type Mode uint

const (
	// AsProgram enables the use of program toolchains.
	AsProgram Mode = 1 << iota

	// AsDockerContainer enables the use of Docker container toolchains.
	AsDockerContainer
)

func (m Mode) String() string {
	var s []string
	if m&AsProgram > 0 {
		s = append(s, "run as program")
	}
	if m&AsDockerContainer > 0 {
		s = append(s, "run as docker container")
	}
	return strings.Join(s, " | ")
}

// Open opens a toolchain by path. The mode parameter controls how it is opened.
func Open(path string, mode Mode) (Toolchain, error) {
	tc, err := Lookup(path)
	if err != nil {
		return nil, err
	}

	if mode&AsProgram > 0 && tc.Program != "" {
		return &programToolchain{filepath.Join(tc.Dir, tc.Program)}, nil
	}
	if mode&AsDockerContainer > 0 && tc.Dockerfile != "" {
		// use current dir as Docker volume mount when running container
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return newDockerToolchain(tc.Path, tc.Dir, tc.Dockerfile, wd)
	}

	if tc.Program != "" || tc.Dockerfile != "" {
		return nil, fmt.Errorf("toolchain %s exists but is not usable in current mode (%s)", path, mode)
	}
	return nil, os.ErrNotExist
}

// A Toolchain is either a local executable program or a Docker container that
// wraps such a program. Toolchains contain tools (as subcommands), which
// perform actions or analysis on a project's source code.
type Toolchain interface {
	// Command returns an *exec.Cmd that will execute this toolchain. Do not use
	// this to execute a tool in this toolchain; use OpenTool instead.
	//
	// Do not modify the returned Cmd's Dir field; some implementations of
	// Toolchain use dir to construct other parts of the Cmd, so it's important
	// that all references to the working directory are consistent.
	Command() (*exec.Cmd, error)

	// Build prepares the toolchain, if needed. For example, for Dockerized
	// toolchains, it builds the Docker image.
	Build() error

	// IsBuilt returns whether the toolchain is built and can be executed (using
	// Command).
	IsBuilt() (bool, error)
}

// A programToolchain is a local executable program toolchain that has been installed in
// the PATH.
type programToolchain struct {
	// program (executable) path
	program string
}

// IsBuilt always returns true for programs.
func (t *programToolchain) IsBuilt() (bool, error) { return true, nil }

// Build is a no-op for programs.
func (t *programToolchain) Build() error { return nil }

// Command returns an *exec.Cmd that executes this program.
func (t *programToolchain) Command() (*exec.Cmd, error) {
	cmd := exec.Command(t.program)
	return cmd, nil
}

// dockerToolchain is a Docker container that wraps a program.
type dockerToolchain struct {
	// dir containing Dockerfile
	dir string

	// dockerfile path
	dockerfile string

	// imageName of the Docker image
	imageName string

	// hostVolumeDir is the host directory to mount at /src in the container.
	hostVolumeDir string

	docker *docker.Client
}

func newDockerToolchain(path, dir, dockerfile, hostVolumeDir string) (*dockerToolchain, error) {
	dockerEndpoint := os.Getenv("DOCKER_HOST")
	if dockerEndpoint == "" {
		dockerEndpoint = "unix:///var/run/docker.sock"
	}
	dc, err := docker.NewClient(dockerEndpoint)
	if err != nil {
		return nil, err
	}

	return &dockerToolchain{
		dir:           dir,
		dockerfile:    dockerfile,
		imageName:     strings.Replace(path, "/", "-", -1),
		docker:        dc,
		hostVolumeDir: hostVolumeDir,
	}, nil
}

// IsBuilt returns whether a Docker image (but perhaps not the most recent one)
// exists that was built from this toolchain's Dockerfile.
func (t *dockerToolchain) IsBuilt() (bool, error) {
	_, err := t.docker.InspectImage(t.imageName)
	if err == docker.ErrNoSuchImage {
		return false, nil
	}
	return err == nil, err
}

// Build builds the Docker image for this toolchain from its Dockerfile.
func (t *dockerToolchain) Build() error {
	cmd := exec.Command("docker", "build", "-t", t.imageName, ".")
	cmd.Dir = t.dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s (command was: %v)", err, cmd.Args)
	}
	return nil
}

// Command returns an *exec.Cmd suitable for executing a command using the
// Docker image's entrypoint.
func (t *dockerToolchain) Command() (*exec.Cmd, error) {
	if built, err := t.IsBuilt(); err != nil {
		return nil, err
	} else if !built {
		if err := t.Build(); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command("docker", "run", "--volume="+t.hostVolumeDir+":/src:ro", t.imageName)
	return cmd, nil
}
