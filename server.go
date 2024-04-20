package main

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	server     *http.Server
	listener   net.Listener
	currentPid int
	quitDaemon = make(chan int64, 1)
)

func startWebServer(ownName string) {
	serverPortStr := strconv.Itoa(int(cfg.HTTPPort))
	log.Printf("Starting web server at %s:%s...\n", cfg.BindIP, serverPortStr)
	listener = startListener(cfg.BindIP, int(cfg.HTTPPort))
	server = &http.Server{Addr: cfg.BindIP + ":" + serverPortStr, Handler: nil}
	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, os.Interrupt, syscall.SIGTERM)
	if len(strings.TrimSpace(cfg.UDSPath)) > 0 {
		go func() {
			udsLis := startUds(cfg.UDSPath)
			if udsLis != nil {
				log.Println("going to serve using unix domain socket on: ", cfg.UDSPath)
				log.Println(server.Serve(udsLis))
			}
		}()
	}
	go func() {
		log.Println(server.Serve(listener))
	}()

	if ownName != "" {
		time.Sleep(2 * time.Second)
		killPreviousProcess(ownName)
	} else {
		log.Println("No process name in config, not trying to kill previous process...")
	}
	log.Println("SERVER_READY", ownName)

	if err := shutdownServer(shutdownSignal); err != nil {
		log.Println("could not close socket, quiting application ; ", err)
	} else {
		log.Println("closed socket, quiting application")
	}
}

func killPreviousProcess(ownName string) bool {
	currentPid = syscall.Getpid()
	log.Println("Own PID:", currentPid)
	log.Printf("Going to attempt to kill running server %s\n", ownName)
	result := false
	err := filepath.Walk("/proc", func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if strings.Count(path, "/") == 3 {
			if strings.Contains(path, "/status") {
				pid, err := strconv.Atoi(path[6:strings.LastIndex(path, "/")])
				if err != nil {
					return nil
				}
				if pid == currentPid {
					return nil
				}
				f, err := ioutil.ReadFile(path)
				if err != nil {
					return nil
				}
				name := string(f[6:bytes.IndexByte(f, '\n')])
				if name == ownName {
					log.Printf("PID: %d, Name: %s will be killed.\n", pid, name)
					proc, err := os.FindProcess(pid)
					if err != nil {
						log.Println(err)
					}
					err = proc.Kill()
					if err != nil {
						log.Println(err)
					} else {
						result = true
					}
					return io.EOF
				}
			}
		}
		return nil
	})
	if err != nil {
		if err == io.EOF {
			// Not an error, just a signal when we are done
			err = nil
		} else {
			log.Println(err)
		}
	}
	return result
}

func shutdownServer(sig chan os.Signal) error {
	select {
	case <-sig:
		log.Println("received kill signal, shutting down server (max ten seconds)")
	case <-quitDaemon:
		log.Println("quiting by internal request, shutting down server (max ten seconds)")
	}
	server.SetKeepAlivesEnabled(false)
	shutdownCtx, cancelFunc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFunc()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Println("result of shutdown ", err)
		return err
	}
	return nil
}
