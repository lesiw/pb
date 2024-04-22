package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"lesiw.io/ctrctl"
	"lesiw.io/flag"
)

const defaultcmd = ".main"

var (
	errParse = errors.New("parse error")

	flags     = flag.NewSet(os.Stderr, "run COMMAND [ARGS...]")
	install   = flags.Bool("i", "install completion scripts")
	list      = flags.Bool("l", "list all commands")
	printroot = flags.Bool("r", "print root")
	verbose   = flags.Bool("v", "verbose")
	printver  = flags.Bool("V,version", "print version")
	usermap   = flags.Strings("u",
		"chowns files based on a given `mapping` (uid:gid::uid:gid)")

	root      string
	container string
	runid     uuid.UUID

	//go:embed version.txt
	versionfile string
	version     string
)

func main() {
	if err := run(); err != nil {
		if !errors.Is(err, errParse) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run() (err error) {
	version = strings.TrimSpace(versionfile)
	if os.Getenv("RUNCTRDEBUG") == "1" {
		ctrctl.Verbose = true
	}
	if err := flags.Parse(os.Args[1:]...); err != nil {
		return errParse
	}
	if *printver {
		fmt.Println(version)
		return nil
	} else if *install {
		return installComp()
	}

	if err = changeToGitRoot(); err != nil {
		return fmt.Errorf("failed to find git root: %s", err)
	}
	if root, err = os.Getwd(); err != nil {
		return fmt.Errorf("failed to get current working directory: %s", err)
	}
	if runid, err = getPbId(); err != nil {
		return err
	}
	if *list {
		return listCommands()
	} else if *printroot {
		fmt.Println(root)
		return nil
	} else if len(*usermap) > 0 {
		return chownFiles(*usermap)
	}
	argv := []string{defaultcmd}
	if len(flags.Args) > 0 {
		argv = flags.Args
	}
	if os.Getenv("RUNCTR") != "" {
		defer containerCleanup()
		if err = containerSetup(); err != nil {
			return err
		}
	}
	return execCommand(argv)
}

func getPbId() (id uuid.UUID, err error) {
	runidfile := filepath.Join(root, ".runid")
	var rawid []byte
	rawid, err = os.ReadFile(runidfile)
	var pe *fs.PathError
	if err == nil {
		uuidstring := strings.TrimSpace(string(rawid))
		if id, err = uuid.Parse(uuidstring); err != nil {
			err = fmt.Errorf("failed to parse project id: %s", err)
			return
		}
		return
	}
	if !errors.As(err, &pe) {
		err = fmt.Errorf("failed to read .runid file: %s", err)
		return
	}
	id = uuid.New()
	err = os.WriteFile(runidfile, []byte(id.String()+"\n"), 0644)
	if err != nil {
		err = fmt.Errorf("failed to write .runid file: %s", err)
		return
	}
	return
}

func execCommand(argv []string) error {
	if container != "" {
		_, err := ctrctl.ContainerExec(
			&ctrctl.ContainerExecOpts{
				Cmd:         attachCmd(),
				Env:         "RUNCTRID=" + container,
				Interactive: true,
				Tty:         isTty(),
			},
			container,
			"run",
			argv...,
		)
		if err != nil {
			return fmt.Errorf("containerized run failed: %s", err)
		}
		return nil
	}
	name := argv[0]
	var args []string
	if len(argv) > 1 {
		args = flags.Args[1:]
	}
	cmdpath, err := findExecutable(name)
	if err != nil {
		if name == defaultcmd {
			fmt.Fprintln(os.Stderr, "bad command. valid commands:")
			return listCommands()
		} else {
			return fmt.Errorf("error running command: %s", err)
		}
	}
	cmd := exec.Command(cmdpath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("error running command: %s", err)
	}
	return nil
}

func changeToGitRoot() error {
	for {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		fileinfo, err := os.Stat(".git")
		if err == nil && fileinfo.IsDir() {
			return nil
		}
		reachedRoot := (cwd == "/" || cwd == (filepath.VolumeName(cwd)+"\\"))
		if reachedRoot || os.Chdir("..") != nil {
			return fmt.Errorf("No .git directory was found.")
		}
	}
}

func findExecutable(name string) (string, error) {
	oldpath := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", oldpath) }()

	runPath := runPath()
	if runPath != "" {
		path := runPath + string(filepath.ListSeparator) + os.Getenv("PATH")
		if err := os.Setenv("PATH", path); err != nil {
			return "", fmt.Errorf("failed to set PATH: %s", err)
		}
	}
	return exec.LookPath(name)
}

func runPath() string {
	paths := os.Getenv("RUNPATH")
	if paths == "" {
		paths = "./bin"
	} else if paths == "-" {
		return ""
	}
	abspaths := strings.Builder{}
	splitpaths := filepath.SplitList(paths)
	sep := string(filepath.Separator)
	for i, path := range splitpaths {
		if i > 0 {
			abspaths.WriteString(string(filepath.ListSeparator))
		}
		parts := strings.Split(path, sep)
		if len(parts) > 0 && parts[0] == "." {
			parts[0] = root
			abspaths.WriteString(strings.Join(parts, sep))
		} else {
			abspaths.WriteString(path)
		}
	}
	return abspaths.String()
}

func listCommands() error {
	paths, err := cmdPaths()
	if err != nil {
		return err
	}
	if len(paths) < 1 {
		fmt.Fprintln(os.Stderr, "<none>")
		return nil
	}
	for _, path := range paths {
		fmt.Println(filepath.Base(path))
	}
	return nil
}

func cmdPaths() (cmds []string, err error) {
	paths := runPath()
	var files []fs.DirEntry
	var info os.FileInfo
	for _, path := range filepath.SplitList(paths) {
		files, err = os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("error reading directory: %s", err)
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			if len(file.Name()) > 0 && file.Name()[0] == '.' {
				continue
			}
			info, err = file.Info()
			if err != nil {
				return
			}
			if info.Mode()&0111 != 0 {
				cmds = append(cmds, filepath.Join(path, file.Name()))
			}
		}
	}
	return
}

func chownFiles(mappings []string) error {
	for _, mapping := range mappings {
		fromstr, tostr, ok := strings.Cut(mapping, "::")
		if !ok {
			return fmt.Errorf("bad format: %s", mapping)
		}

		fuidstr, fgidstr, ok := strings.Cut(fromstr, ":")
		if !ok {
			return fmt.Errorf("bad user: %s", fromstr)
		}
		tuidstr, tgidstr, ok := strings.Cut(tostr, ":")
		if !ok {
			return fmt.Errorf("bad user: %s", tostr)
		}

		fuid, err := strconv.Atoi(fuidstr)
		if err != nil {
			return fmt.Errorf("bad id: %s", fromstr)
		}
		fgid, err := strconv.Atoi(fgidstr)
		if err != nil {
			return fmt.Errorf("bad id: %s", fromstr)
		}
		tuid, err := strconv.Atoi(tuidstr)
		if err != nil {
			return fmt.Errorf("bad id: %s", fromstr)
		}
		tgid, err := strconv.Atoi(tgidstr)
		if err != nil {
			return fmt.Errorf("bad id: %s", fromstr)
		}

		if err = chownDir(root, fuid, fgid, tuid, tgid); err != nil {
			return err
		}
	}
	return nil
}