/*
Soldier is responsible for receiving commands from commander and doing tasks such as starting nodes, uploading logs.

   cd harmony-benchmark/bin
   go build -o soldier ../aws-experiment-launch/experiment/soldier/main.go
   ./soldier -ip={node_ip} -port={node_port}
*/
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/simple-rules/experiment-deploy/experiment/soldier/s3"
	"github.com/simple-rules/experiment-deploy/experiment/utils"
	globalUtils "github.com/simple-rules/harmony-benchmark/utils"
)

type initReq struct {
	Ip            string `json:"ip"`
	Port          string `json:"port"`
	BenchmarkArgs string `json:"benchmarkArgs"`
	TxgenArgs     string `json:"txgenArgs"`
}

type updateReq struct {
	Bucket string `json:"bucket"`
	Folder string `json:"folder"`
	File   string `json:"file"`
}

type configReq struct {
	SessionID string `json:"sessionId"`
	ConfigURL string `json:"configURL"`
}

var (
	version string
	builtBy string
	builtAt string
	commit  string
)

type soliderSetting struct {
	ip                string
	port              string
	metricsProfileURL string
}

type sessionInfo struct {
	id                  string
	commanderIP         string
	commanderPort       string
	localConfigFileName string
	logFolder           string
	config              *globalUtils.DistributionConfig
	myConfig            globalUtils.ConfigEntry
	txgenAdditionalArgs []string
	nodeAdditionalArgs  []string
}

const (
	bucketName      = "richard-bucket-test"
	logFolderPrefix = "../tmp_log/"
)

var (
	setting       soliderSetting
	globalSession sessionInfo
	txgenArgs     = []string{
		"-config_file",
		"-max_num_txs_per_batch",
		"-log_folder",
		"-numSubset",
		"-duration",
		"-version",
		"-cross_shard_ratio",
	}
)

func printVersion(me string) {
	fmt.Fprintf(os.Stderr, "Harmony (C) 2018. %v, version %v-%v (%v %v)\n", path.Base(me), version, commit, builtBy, builtAt)
	os.Exit(0)
}

func socketServer() {
	soldierPort := "1" + setting.port // the soldier port is "1" + node port
	listen, err := net.Listen("tcp4", ":"+soldierPort)
	if err != nil {
		log.Fatalf("Socket listen port %s failed,%s", soldierPort, err)
		os.Exit(1)
	}
	defer listen.Close()
	log.Printf("Begin listen for command on port: %s", soldierPort)

	for {
		conn, err := listen.Accept()
		if err != nil {
			log.Fatalln(err)
			continue
		}
		go handler(conn)
	}
}

func handler(conn net.Conn) {
	defer conn.Close()

	var (
		buf = make([]byte, 1024)
		r   = bufio.NewReader(conn)
		w   = bufio.NewWriter(conn)
	)

ILOOP:
	for {
		n, err := r.Read(buf)
		data := string(buf[:n])

		switch err {
		case io.EOF:
			break ILOOP
		case nil:
			log.Println("Received command", data)

			handleCommand(data, w)

			log.Println("Waiting for new command...")

		default:
			log.Fatalf("Receive data failed:%s", err)
			return
		}
	}
}

func handleCommand(command string, w *bufio.Writer) {
	args := strings.Split(command, " ")

	if len(args) <= 0 {
		return
	}

	switch command := args[0]; command {
	case "ping":
		{
			handlePingCommand(w)
		}
	case "init":
		{
			handleInitCommand(args[1:], w)
		}
	case "kill":
		{
			handleKillCommand(w)
		}
	case "log":
		{
			handleLogCommand(w)
		}
	case "log2":
		{
			handleLog2Command(w)
		}
	case "update":
		{
			handleUpdateCommand(args[1:], w)
		}
	}
}

func handleInitCommand(args []string, w *bufio.Writer) {
	// init ip port config_file sessionID
	log.Println("Init command", args)

	// read arguments
	ip := args[0]
	globalSession.commanderIP = ip
	port := args[1]
	globalSession.commanderPort = port
	configURL := args[2]
	sessionID := args[3]
	globalSession.nodeAdditionalArgs = nil
	globalSession.txgenAdditionalArgs = nil
	if len(args) > 4 {
		additionalArgs := args[4:]
		i := 0
		for i < len(additionalArgs) {
			if Include(txgenArgs, additionalArgs[i]) {
				globalSession.txgenAdditionalArgs = append(globalSession.txgenAdditionalArgs, additionalArgs[i:i+2]...)
			} else {
				globalSession.nodeAdditionalArgs = append(globalSession.nodeAdditionalArgs, additionalArgs[i:i+2]...)
			}
			i += 2
		}
	}

	globalSession.id = sessionID
	globalSession.logFolder = fmt.Sprintf("%slog-%v", logFolderPrefix, sessionID)

	globalSession.config = globalUtils.NewDistributionConfig()

	// create local config file
	globalSession.localConfigFileName = fmt.Sprintf("node_config_%v_%v.txt", setting.port, globalSession.id)
	utils.DownloadFile(globalSession.localConfigFileName, configURL)
	log.Println("Successfully downloaded config")

	globalSession.config.ReadConfigFile(globalSession.localConfigFileName)
	myConfig := globalSession.config.GetMyConfigEntry(setting.ip, setting.port)
	if myConfig == nil {
		logAndReply(w, "Failed.")
		return
	}

	globalSession.myConfig = *myConfig

	if err := runInstance(); err == nil {
		logAndReply(w, "Done init.")
	} else {
		logAndReply(w, "Failed.")
	}
}

func handleKillCommand(w *bufio.Writer) {
	log.Println("Kill command")
	if err := killPort(setting.port); err == nil {
		logAndReply(w, "Done kill.")
	} else {
		logAndReply(w, "Failed.")
	}
}

func killPort(port string) error {
	if runtime.GOOS == "windows" {
		command := fmt.Sprintf("(Get-NetTCPConnection -LocalPort %s).OwningProcess -Force", port)
		return globalUtils.RunCmd("Stop-Process", "-Id", command)
	}
	command := fmt.Sprintf("lsof -i tcp:%s | grep LISTEN | awk '{print $2}' | xargs kill -9", port)
	return globalUtils.RunCmd("/bin/bash", "-c", command)
}

func handlePingCommand(w *bufio.Writer) {
	log.Println("Ping command")
	logAndReply(w, "I'm alive")
}

func handleLogCommand(w *bufio.Writer) {
	log.Println("Log command")

	files, err := ioutil.ReadDir(globalSession.logFolder)
	if err != nil {
		logAndReply(w, fmt.Sprintf("Failed to read log folder. Error: %s", err.Error()))
		return
	}

	filePaths := make([]string, len(files))
	for i, f := range files {
		filePaths[i] = fmt.Sprintf("%s/%s", globalSession.logFolder, f.Name())
	}

	req, err := newUploadFileRequest(
		fmt.Sprintf("http://%s:%s/upload", globalSession.commanderIP, globalSession.commanderPort),
		"file",
		filePaths,
		nil)
	if err != nil {
		logAndReply(w, fmt.Sprintf("Failed to create upload request. Error: %s", err.Error()))
		return
	}
	client := &http.Client{}
	_, err = client.Do(req)
	if err != nil {
		logAndReply(w, fmt.Sprintf("Failed to upload log. Error: %s", err.Error()))
		return
	}
	logAndReply(w, "Upload log done!")
}

// Creates a new file upload http request with optional extra params
func newUploadFileRequest(uri string, paramName string, paths []string, params map[string]string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		part, err := writer.CreateFormFile(paramName, filepath.Base(path))
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(part, file)
		log.Printf(path)
	}

	for key, val := range params {
		_ = writer.WriteField(key, val)
	}
	err := writer.Close()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, err
}

func logAndReply(w *bufio.Writer, message string) {
	log.Println(message)
	w.Write([]byte(message))
	w.Flush()
}

func handleLog2Command(w *bufio.Writer) {
	log.Println("Log command")

	files, err := ioutil.ReadDir(globalSession.logFolder)
	if err != nil {
		logAndReply(w, fmt.Sprintf("Failed to create log folder. Error: %s", err.Error()))
		return
	}

	filePaths := make([]string, len(files))
	for i, f := range files {
		filePaths[i] = fmt.Sprintf("%s/%s", globalSession.logFolder, f.Name())
	}

	// TODO: currently only upload the first file.
	_, err = s3.UploadFile(bucketName, filePaths[0], strings.Replace(filePaths[0], logFolderPrefix, "", 1))
	if err != nil {
		logAndReply(w, fmt.Sprintf("Failed to create upload request. Error: %s", err.Error()))
		return
	}
	logAndReply(w, "Upload log done!")
}

func handleUpdateCommand(args []string, w *bufio.Writer) {
	log.Println("Update command")

	var updateFiles = []string{
		"benchmark",
		"txgen",
		"md5sum.txt",
		"commander",
		"soldier",
		"md5sum-cs.txt",
	}

	var baseURL string
	if len(args) == 0 {
		// default bucket
		baseURL = "http://unique-bucket-bin.s3.amazonaws.com"
	}
	if len(args) == 1 {
		baseURL = fmt.Sprintf("http://%v.s3.amazonaws.com", args[0])
	}
	if len(args) == 2 {
		baseURL = fmt.Sprintf("http://%v.s3.amazonaws.com/%s", args[0], args[1])
	}
	count := 0
	for _, f := range updateFiles {
		fileURL := fmt.Sprintf("%v/%v", baseURL, f)
		if err := utils.DownloadFile(f, fileURL); err != nil {
			log.Println("Update failed: ", f)
			count++
		} else {
			if !strings.HasSuffix(f, ".txt") {
				os.Chmod(f, 0755)
			}
			log.Println("Update succeeded: ", f)
		}
	}

	logAndReply(w, fmt.Sprintf("Done Update %v binaries", count))
}

func runInstance() error {
	os.MkdirAll(globalSession.logFolder, os.ModePerm)

	if globalSession.myConfig.Role == "client" {
		return runClient()
	}
	return runNode()
}

func runNode() error {
	log.Println("running instance")
	args :=
		append([]string{"-ip", setting.ip, "-port", setting.port, "-config_file", globalSession.localConfigFileName, "-log_folder", globalSession.logFolder}, globalSession.nodeAdditionalArgs...)
	return globalUtils.RunCmd("./benchmark", args...)
}

func runClient() error {
	log.Println("running client")
	args :=
		append([]string{"-config_file", globalSession.localConfigFileName, "-log_folder", globalSession.logFolder}, globalSession.txgenAdditionalArgs...)
	return globalUtils.RunCmd("./txgen", args...)
}

func Index(vs []string, t string) int {
	for i, v := range vs {
		if v == t {
			return i
		}
	}
	return -1
}

func Include(vs []string, t string) bool {
	return Index(vs, t) >= 0
}

func initHandler(w http.ResponseWriter, r *http.Request) {
	var res string
	if r.Method != http.MethodGet {
		res = "Not Supported Method"
		io.WriteString(w, res)
		return
	}
	log.Println("Init Handler")
	if r.Body == nil {
		http.Error(w, "no data found in the init request", http.StatusBadRequest)
		return
	}

	var init initReq

	err := json.NewDecoder(r.Body).Decode(&init)
	if err != nil {
		log.Printf("Json decode failed %v\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	globalSession.txgenAdditionalArgs = append(globalSession.txgenAdditionalArgs, init.TxgenArgs)
	globalSession.nodeAdditionalArgs = append(globalSession.nodeAdditionalArgs, init.BenchmarkArgs)
	if err := runInstance(); err == nil {
		res = "Done init"
	} else {
		res = fmt.Sprintf("Failed init %v", err)
	}
	io.WriteString(w, res)
}

func pingHandler(w http.ResponseWriter, r *http.Request) {
	var res string
	if r.Method != http.MethodGet {
		res = "Not Supported Method"
		io.WriteString(w, res)
		return
	}
	log.Println("Ping Handler")
	res = "Succeeded"
	io.WriteString(w, res)
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	var res string
	if r.Method != http.MethodGet {
		res = "Not Supported Method"
		io.WriteString(w, res)
		return
	}
	log.Println("Update Handler")

	if r.Body == nil {
		http.Error(w, "no data found in the update request", http.StatusBadRequest)
		return
	}

	var update updateReq

	err := json.NewDecoder(r.Body).Decode(&update)
	if err != nil {
		log.Printf("Json decode failed %v\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	downloadURL := fmt.Sprintf("http://%v.s3.amazonaws.com/%v/%v", update.Bucket, update.Folder, update.File)
	if err := utils.DownloadFile(update.File, downloadURL); err != nil {
		log.Println("Update failed: ", downloadURL)
		res = "Failed"
	} else {
		res = "Succeeded"
	}
	io.WriteString(w, res)
}

func killHandler(w http.ResponseWriter, r *http.Request) {
	var res string
	if r.Method != http.MethodGet {
		res = "Not Supported Method"
		io.WriteString(w, res)
		return
	}
	log.Println("Kill Handler")
	if err := killPort(setting.port); err == nil {
		res = "Succeeded"
	} else {
		res = "Failed"
	}
	io.WriteString(w, res)
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	var res string
	if r.Method != http.MethodGet {
		res = "Not Supported Method"
		io.WriteString(w, res)
		return
	}
	log.Println("Config Handler")

	if r.Body == nil {
		http.Error(w, "no data found in the config request", http.StatusBadRequest)
		return
	}

	var config configReq

	err := json.NewDecoder(r.Body).Decode(&config)
	if err != nil {
		log.Printf("Json decode failed %v\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	globalSession.id = config.SessionID
	globalSession.logFolder = fmt.Sprintf("%slog-%v", logFolderPrefix, config.SessionID)
	globalSession.config = globalUtils.NewDistributionConfig()

	// create local config file
	globalSession.localConfigFileName = fmt.Sprintf("node_config_%v_%v.txt", setting.port, globalSession.id)
	if err := utils.DownloadFile(globalSession.localConfigFileName, config.ConfigURL); err != nil {
		log.Println("Failed to downloaded config", config.ConfigURL)
		res = "Failed to download"
	} else {
		log.Println("Successfully downloaded config", config.ConfigURL)
		globalSession.config.ReadConfigFile(globalSession.localConfigFileName)
		myConfig := globalSession.config.GetMyConfigEntry(setting.ip, setting.port)
		if myConfig == nil {
			res = "Failed to get myconfig"
		} else {
			globalSession.myConfig = *myConfig
			res = "Succeeded"
		}
	}
	io.WriteString(w, res)
}

func httpServer() {
	http.HandleFunc("/init", initHandler)
	http.HandleFunc("/ping", pingHandler)
	http.HandleFunc("/update", updateHandler)
	http.HandleFunc("/kill", killHandler)
	http.HandleFunc("/config", configHandler)

	s := http.Server{
		Addr:           fmt.Sprintf("%s:1%v", setting.ip, setting.port),
		Handler:        nil,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	log.Printf("HTTP server listen on port: 1%v", setting.port)
	log.Println("Supported API:")
	log.Println("/ping\t\t\tI'm alive!")
	log.Println("/config\t\t\tDownload distribution config file.")
	log.Println("/init\t\t\tStart Benchmark/Txgen")
	log.Println("/update\t\t\tDownload/Update binary")
	log.Println("/kill\t\t\tKill Running Benchmark/Txgen")

	log.Fatalf(fmt.Sprintf("http server error: %v", s.ListenAndServe()))
}

func main() {
	ip := flag.String("ip", "127.0.0.1", "IP of the node.")
	port := flag.String("port", "9000", "port of the node.")
	versionFlag := flag.Bool("version", false, "Output version info")
	http := flag.Bool("http", false, "Start in http server mode")

	flag.Parse()

	if *versionFlag {
		printVersion(os.Args[0])
	}

	setting.ip = *ip
	setting.port = *port

	if *http {
		httpServer()
	} else {
		socketServer()
	}
}