package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/automata-network/attestable-build-tool/build"
	"github.com/automata-network/attestable-build-tool/misc"
	"github.com/chzyer/logex"
	"github.com/hf/nitrite"
	"github.com/mdlayher/vsock"
)

type BuildToolBuild struct {
	Dir    string `default:"."`
	Listen string `default:"vsock://:0"`
	Vendor string
	Nitro  string
	Mem    string `default:"12288"`
	Cid    int
	Cpu    int `default:"2"`
	Output string
	Nonce  string
	Debug  bool

	Server *http.Server `flagly:"-"`
}

func (b *BuildToolBuild) FlaglyHandle() error {
	if b.Nonce == "" {
		var nonce [16]byte
		rand.Read(nonce[:])
		b.Nonce = fmt.Sprintf("%x", nonce)
	}

	if err := os.Chdir(b.Dir); err != nil {
		return logex.Trace(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return logex.Trace(err)
	}
	cid := uint32(b.Cid)

	manifest, err := build.NewManifest("build.json")
	if err != nil {
		return logex.Trace(err, "build.json is required")
	}

	if b.Vendor == "" {
		b.Vendor = fmt.Sprintf("ata-build-%v", strings.ToLower(manifest.Language))
	}
	if b.Nitro == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		b.Nitro = filepath.Join(home, fmt.Sprintf("ata-build-%v-latest.eif", strings.ToLower(manifest.Language)))
	}
	if b.Output == "" {
		b.Output = filepath.Base(cwd)
		if b.Output == "/" {
			logex.Fatal("-output is required")
		}
	}
	if cid == 0 {
		randCid := make([]byte, 4)
		rand.Read(randCid)
		cid = binary.LittleEndian.Uint32(randCid)
		if cid == 0 {
			logex.Fatal("fail to generate cid")
		}
		cid |= 1 << 31
	}

	listener, runServerWait, err := b.RunServer()
	if err != nil {
		logex.Fatal(err)
	}

	go func() {
		if err := runServerWait(); err != nil {
			logex.Fatal(err)
		}
	}()

	var vendorTars [][2]string

	if manifest.Input.Vendor != "" {
		vendorDir, err := os.MkdirTemp("", "vendor*")
		if err != nil {
			return logex.Trace(err)
		}
		defer os.RemoveAll(vendorDir)

		if err := misc.Exec(nil, "docker", "run", "--rm",
			"-v", fmt.Sprintf("%v:/tmp/vendor", vendorDir),
			"-v", fmt.Sprintf("%v:/workspace/code", cwd),
			b.Vendor, "attestable-build-tool", "vendor", "-dir", "/workspace/code",
		); err != nil {
			return logex.Trace(err)
		}
		dir, err := os.ReadDir(vendorDir)
		if err != nil {
			return logex.Trace(err)
		}
		for _, vendor := range dir {
			vendorTars = append(vendorTars, [2]string{
				filepath.Join(vendorDir, vendor.Name()),
				"/usr/local/",
			})
		}
	}

	logex.Infof("pkg codes: %v", b.Dir)
	if err := os.Chdir(b.Dir); err != nil {
		return logex.Trace(err)
	}
	tarFile, err := misc.Tar(nil, "sourcecode", []string{"."})
	if err != nil {
		return logex.Trace(err)
	}
	defer func() {
		os.Remove(tarFile)
	}()

	tarFd, err := os.Open(tarFile)
	if err != nil {
		return logex.Trace(err)
	}
	logex.Info("package tar file to:", tarFd.Name())
	defer tarFd.Close()

	targetFile, err := os.OpenFile(b.Output+".tar", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return logex.Trace(err)
	}
	defer targetFile.Close()

	var cmd *exec.Cmd
	var client *http.Client
	var endpoint string
	if b.Nitro != "" {
		// misc.Exec(nil, "nitro-cli", "terminate-enclave", "--all")
		cmd = misc.RunNitroEnclave(b.Nitro, b.Mem, uint(b.Cpu), cid, b.Debug)
		if err := cmd.Start(); err != nil {
			return logex.Trace(err)
		}
		client = misc.NewVsockClient(nil)
		endpoint = fmt.Sprintf("http://%v:12345", cid)
	} else {
		// local mode
		cmd = exec.Command(os.Args[0], "worker", "-listen", "tcp://localhost:12345")
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		if err := cmd.Start(); err != nil {
			return logex.Trace(err)
		}
		client = http.DefaultClient
		endpoint = "http://localhost:12345"
	}

	vsockPort, _ := strconv.Atoi(strings.Split(listener.Addr().String(), ":")[1])
	vsockId, err := vsock.ContextID()
	if err != nil {
		return logex.Trace(err)
	}

	for {
		_, err := client.Get(endpoint + "/ping?" + url.Values{
			"host": {fmt.Sprintf("%v:%v", vsockId, vsockPort)},
		}.Encode())
		if err != nil {
			logex.Errorf("connecting to the enclave... retry in 5secs")
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	for _, tf := range vendorTars {
		fd, err := os.Open(tf[0])
		if err != nil {
			return logex.Trace(err)
		}
		query := url.Values{"target": {tf[1]}}
		response, err := client.Post(endpoint+"/vendor?"+query.Encode(), "application/octet-stream", fd)
		fd.Close()
		if err != nil {
			return logex.Trace(err)
		}
		if err := checkResponseError(response); err != nil {
			return logex.Trace(err)
		}
	}

	query := url.Values{"nonce": {b.Nonce}}
	response, err := client.Post(endpoint+"/build?"+query.Encode(), "application/octet-stream", tarFd)
	tarFd.Close()
	if err != nil {
		return logex.Trace(err)
	}

	defer cmd.Wait()
	defer response.Body.Close()
	if err := checkResponseError(response); err != nil {
		return logex.Trace(err)
	}

	if _, err := io.Copy(targetFile, response.Body); err != nil {
		return logex.Trace(err)
	}

	report := response.Header.Get("Report")
	if report != "" {
		reportBytes, err := base64.URLEncoding.DecodeString(report)
		if err != nil {
			logex.Error("decode report fail:", err)
		}

		report, err := nitrite.Verify(reportBytes, nitrite.VerifyOptions{
			CurrentTime: time.Now(),
		})
		if err != nil {
			return logex.Trace(err)
		}

		if err := os.WriteFile(b.Output+".report", reportBytes, 0666); err != nil {
			logex.Error(err)
		}
		dst := bytes.NewBuffer(nil)
		dst.WriteString("## Attestation Report\n")
		dst.WriteString("**PCR0**: \n `0x" + hex.EncodeToString(report.Document.PCRs[0]) + "`\n")
		dst.WriteString("\n**Report User Data**:\n")
		dst.WriteString("```\n")
		if err := json.Indent(dst, report.Document.UserData, "", "\t"); err != nil {
			logex.Error(err)
		}
		dst.WriteString("\n```\n")
		if err := os.WriteFile(b.Output+".txt", dst.Bytes(), 0666); err != nil {
			logex.Error(err)
		}
	}
	logex.Info("save file to:", targetFile.Name())

	return nil
}

func (b *BuildToolBuild) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	switch req.URL.Path {
	case "/log":
		data, err := io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprint(w, err.Error())
			return
		}
		fmt.Printf("enclave: %s", data)
	}
}

func (b *BuildToolBuild) RunServer() (net.Listener, func() error, error) {
	uri, err := url.Parse(b.Listen)
	if err != nil {
		return nil, nil, logex.Trace(err)
	}
	listener, err := misc.Listen(uri)
	if err != nil {
		return nil, nil, logex.Trace(err)
	}

	b.Server = &http.Server{
		Addr:    uri.Host,
		Handler: b,
	}

	return listener, func() error {
		defer listener.Close()
		if err := b.Server.Serve(listener); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				return logex.Trace(err)
			}
		}
		return nil
	}, nil
}

func checkResponseError(response *http.Response) error {
	if response.StatusCode != 200 {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return logex.Trace(err)
		}
		return logex.NewErrorf("%s", body)
	}
	return nil
}

type BuildMode string

var (
	NitroBuildMode  BuildMode = "nitro"
	DockerBuildMode BuildMode = "docker"
	LocalBuildMode  BuildMode = "local"
)
