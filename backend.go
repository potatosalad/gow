package main

import (
	"errors"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"
	"path/filepath"
	"strconv"
	"time"
	"bytes"
	"io"
	"strings"

	"github.com/joho/godotenv"
)

type Backend struct {
	appPath      string
	port         int
	process      *os.Process
	startedAt    time.Time
	exited       bool
	exitChan 		 chan interface{}
	activityChan chan interface{}
}

func (b *Backend) Close() {
	log.Println("Terminating", b.appPath, "pid", b.process.Pid)

	err := b.process.Signal(syscall.SIGTERM)
	if err != nil {
		log.Println("failed to kill process: ", err)
		return
	}

	<-b.exitChan

	log.Println("Terminated", b.appPath)
}

func (b *Backend) IsRestartRequested() bool {
	if b.exited {
		return true
	}
	fi, err := os.Stat(b.appPath + "/tmp/restart.txt")
	if err != nil {
		return false
	}
	return fi.ModTime().After(b.startedAt)
}

type BootCrash struct {
	Log bytes.Buffer
	Env []string
	Cmd string
	Path string
}
func (b BootCrash) Error() string {
	return "app crashed during boot"
}

func SpawnBackend(appName string) (*Backend, error) {
	pathToApp, err := appDir(appName)
	if err != nil {
		return nil, err
	}
	port, err := getFreeTCPPort()
	if err != nil {
		return nil, err
	}
	log.Println("Spawning", pathToApp, "on port", port)

	env := os.Environ()

	pathbytes, err := ioutil.ReadFile(os.Getenv("HOME") + "/.pow/.path")
	path := os.Getenv("PATH")
	if err == nil {
		path = string(pathbytes)
	} else {
		log.Println("while reading path file:", err)
	}
	// remove the old PATH
	for i, v := range env {
		if strings.Index(v, "PATH=") == 0 {
			env = append(env[:i], env[i+1:]...)
		}
	}
	env = append(env, "PATH="+path, "PORT="+strconv.Itoa(port))

	// add .env
	entries, err := godotenv.Read(pathToApp+"/.env")
	if err != nil {
		return nil, err
	}
	for k, v := range entries {
		env = append(env, k+"="+v)
	}

	procfile, err := ReadProcfile(pathToApp+"/Procfile")
	if err != nil {
		return nil, err
	}
	var CmdName string
	for _, v := range procfile.Entries {
		if v.Name == "web" {
			CmdName = v.Command
		}
	}

	if CmdName == "" {
		return nil, errors.New("No 'web' entry found in Procfile")
	}

	cmd := exec.Command("bash", "-c", "exec "+CmdName)

	var bootlog bytes.Buffer

	toStderrWithCapture := io.MultiWriter(os.Stderr, &bootlog)

	cmd.Stdout = toStderrWithCapture // never write to gowd's stdout
	cmd.Stderr = toStderrWithCapture
	cmd.Dir = pathToApp
	cmd.Env = env

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	exitChan := make(chan interface{}, 1)
	b := &Backend{appPath: pathToApp, port: port, process: cmd.Process, startedAt: time.Now(), activityChan: make(chan interface{}), exitChan: exitChan}
	booting := true
	crashChan := make(chan error, 1)
	go func() {
		cmd.Wait()
		b.exited = true
		b.exitChan <- new(interface{})

		if booting {
			crashChan <- BootCrash{Log: bootlog, Env: env, Cmd: CmdName, Path: pathToApp}
		}
	}()

	log.Println("waiting for spawn result for", pathToApp)

	select {
	case <-awaitTCP(b.Address()):
		log.Println(pathToApp, "came up successfully")
		booting = false
		go b.watchForActivity()

		return b, nil
	case <-time.After(30 * time.Second):
		log.Println(pathToApp, "failed to bind")
		cmd.Process.Kill()
		return nil, errors.New("app failed to bind")
	case err := <-crashChan:
		log.Println(pathToApp, "crashed while starting")
		return nil, err
	}
}

func (b *Backend) Touch() {
	if b.activityChan != nil {
		b.activityChan <- new(interface{})
	}
}

func (b *Backend) Address() string {
	return "127.0.0.1:" + strconv.Itoa(b.port)
}

// Close the backend after inactivity
func (b *Backend) watchForActivity() {
outer:
	for {
		select {
		case _, ok := <-b.activityChan:
			if ok {
				continue
			} else {
				b.Close()
				b.activityChan = nil
				break outer
			}

		case <-time.After(30 * time.Minute):
			log.Println(b.appPath, "backend idling.")
			b.Close()
			b.activityChan = nil
			break outer
		}
	}
}

func awaitTCP(address string) chan bool {
	c := make(chan bool)
	go func() {
		for {
			_, err := net.Dial("tcp", address)
			if err == nil {
				c <- true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return c
}

func getFreeTCPPort() (port int, err error) {
	// We still have a small race condition here, but meh.
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func appDir(name string) (path string, err error) {
	path, err = filepath.EvalSymlinks(os.Getenv("HOME") + "/.pow/" + name)
	return
}
