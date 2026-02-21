package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// bindRegex: Captures Windows volume mounts.
	// Matches "C:\data:/app" -> Group 1: C, Group 2: data, Group 3: :/app
	bindRegex    = regexp.MustCompile(`^([a-zA-Z]):[\\/]([^:]+)(.*)`)
	
	// envPathRegex: Identifies Windows paths inside environment variables.
	// Matches "C:\config\db"
	envPathRegex = regexp.MustCompile(`^([a-zA-Z]):[\\/](.*)`)
	
	// Registry to prevent binding the same port multiple times
	activeForwards = make(map[string]bool)
	forwardMutex   sync.Mutex
)

// translateWinPath transforms C:\Folder to /mnt/hgfs/docker/volumes/C/Folder
func translateWinPath(basePath, drive, path string) string {
	drive = strings.ToUpper(drive)
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	return fmt.Sprintf("%s%s/%s", basePath, drive, path)
}

func main() {
	// CLI ARGUMENTS
	targetIP := flag.String("ip", "192.168.137.25", "The IP of the remote VM")
	vmPort := flag.String("vm-port", "2375", "The Docker port on the VM")
	proxyPort := flag.String("local-port", "2375", "The port to listen on locally (127.0.0.1)")
	basePath := flag.String("base", "/mnt/hgfs/docker/volumes/", "Prefix for paths in the VM")
	flag.Parse()

	targetAddr := fmt.Sprintf("%s:%s", *targetIP, *vmPort)
	localAddr := fmt.Sprintf("127.0.0.1:%s", *proxyPort)

	// 1. CUSTOM TRANSPORT
	// We define a transport with timeouts to handle VM pauses or network hiccups gracefully.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext(ctx, "tcp", targetAddr)
		},
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
	}

	// 2. REVERSE PROXY LOGIC
	// The Director modifies the request before it is sent to the VM.
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = targetAddr
			req.Host = targetAddr // Necessary so the VM sees a consistent Host header

			// Intercept 'Container Create' to rewrite paths and discover ports
			if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/containers/create") {
				modifyAndExtract(req, *basePath, *targetIP)
			}
		},
		Transport: transport,
	}

	// 3. START TCP SERVER
	log.Printf("-------------------------------------------------------")
	log.Printf("DOCKER BRIDGE PROXY: tcp://%s -> tcp://%s", localAddr, targetAddr)
	log.Printf("Path Mapping: Windows -> %s{DRIVE}/", *basePath)
	log.Printf("-------------------------------------------------------")

	if err := http.ListenAndServe(localAddr, proxy); err != nil {
		log.Fatalf("Proxy failed to start: %v", err)
	}
}

// modifyAndExtract handles the heavy lifting of JSON manipulation and port triggering.
func modifyAndExtract(req *http.Request, basePath string, targetIP string) {
	if req.Body == nil || req.Body == http.NoBody {
		return
	}

	// Read original body and close it
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return
	}
	req.Body.Close()

	var containerMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &containerMap); err != nil {
		// If it's not JSON, put the body back and ignore
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		return
	}

	modified := false

	// REWRITE HOSTCONFIG (Binds & Port Discovery)
	if hostConfig, ok := containerMap["HostConfig"].(map[string]interface{}); ok {
		// Rewriting Volume Bindings
		if binds, ok := hostConfig["Binds"].([]interface{}); ok {
			for i, b := range binds {
				if s, ok := b.(string); ok {
					if match := bindRegex.FindStringSubmatch(s); match != nil {
						newPath := translateWinPath(basePath, match[1], match[2]) + match[3]
						binds[i] = newPath
						log.Printf("[PATH] Rewrote Bind: %s -> %s", s, newPath)
						modified = true
					}
				}
			}
		}

		// Discovering Ports to forward (e.g., Supabase DB/API)
		if portBindings, ok := hostConfig["PortBindings"].(map[string]interface{}); ok {
			for _, bindings := range portBindings {
				if bList, ok := bindings.([]interface{}); ok {
					for _, b := range bList {
						if bMap, ok := b.(map[string]interface{}); ok {
							if hostPort, ok := bMap["HostPort"].(string); ok {
								go startForwarder(hostPort, targetIP)
							}
						}
					}
				}
			}
		}
	}

	// REWRITE ENVIRONMENT VARIABLES
	if envs, ok := containerMap["Env"].([]interface{}); ok {
		for i, e := range envs {
			if s, ok := e.(string); ok {
				kv := strings.SplitN(s, "=", 2)
				if len(kv) == 2 && envPathRegex.MatchString(kv[1]) {
					match := envPathRegex.FindStringSubmatch(kv[1])
					newEnv := kv[0] + "=" + translateWinPath(basePath, match[1], match[2])
					envs[i] = newEnv
					log.Printf("[ENV]  Rewrote: %s -> %s", s, envs[i])
					modified = true
				}
			}
		}
	}

	// RECONSTRUCT REQUEST
	var finalBytes []byte
	if modified {
		finalBytes, _ = json.Marshal(containerMap)
		log.Printf("[OK] Payload modified for VM compatibility.")
	} else {
		finalBytes = bodyBytes
	}

	// Replace the request body with our modified version
	req.Body = io.NopCloser(bytes.NewBuffer(finalBytes))
	req.ContentLength = int64(len(finalBytes))
}

// startForwarder bridges a local port to the VM port (Application traffic)
func startForwarder(port string, targetIP string) {
	forwardMutex.Lock()
	if activeForwards[port] {
		forwardMutex.Unlock()
		return
	}
	activeForwards[port] = true
	forwardMutex.Unlock()

	localAddr := fmt.Sprintf("127.0.0.1:%s", port)
	remoteAddr := fmt.Sprintf("%s:%s", targetIP, port)

	l, err := net.Listen("tcp", localAddr)
	if err != nil {
		log.Printf("[TUNNEL] Skipping port %s: already in use locally.", port)
		return
	}
	log.Printf("[TUNNEL] Local bridge created: %s -> %s", localAddr, remoteAddr)

	for {
		client, err := l.Accept()
		if err != nil { continue }

		go func(c net.Conn) {
			defer c.Close()
			s, err := net.DialTimeout("tcp", remoteAddr, 5*time.Second)
			if err != nil { return }
			defer s.Close()

			// Fast-copy data between local client and VM server
			done := make(chan bool)
			go func() { io.Copy(s, c); done <- true }()
			go func() { io.Copy(c, s); done <- true }()
			<-done
		}(client)
	}
}