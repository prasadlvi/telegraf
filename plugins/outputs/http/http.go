package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/influxdata/telegraf/agent"
	"github.com/influxdata/telegraf/internal/config"
	"github.com/kardianos/osext"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/serializers"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	defaultURL = "http://127.0.0.1:8080/telegraf"
)

var revision = ""
var testContext context.Context = nil

var sampleConfig = `
  ## URL is the address to send metrics to
  url = "http://127.0.0.1:8080/telegraf"

  ## Timeout for HTTP message
  # timeout = "5s"

  ## HTTP method, one of: "POST" or "PUT"
  # method = "POST"

  ## HTTP Basic Auth credentials
  # username = "username"
  # password = "pa$$word"

  ## OAuth2 Client Credentials Grant
  # client_id = "clientid"
  # client_secret = "secret"
  # token_url = "https://indentityprovider/oauth2/v1/token"
  # scopes = ["urn:opc:idm:__myscopes__"]

  ## Optional TLS Config
  # tls_ca = "/etc/telegraf/ca.pem"
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"
  ## Use TLS but skip chain & host verification
  # insecure_skip_verify = false

  ## Data format to output.
  ## Each data format has it's own unique set of configuration options, read
  ## more about them here:
  ## https://github.com/influxdata/telegraf/blob/master/docs/DATA_FORMATS_OUTPUT.md
  # data_format = "influx"

  ## HTTP Content-Encoding for write request body, can be set to "gzip" to
  ## compress body or "identity" to apply no encoding.
  # content_encoding = "identity"

  ## Additional HTTP headers
  # [outputs.http.headers]
  #   # Should be set manually to "application/json" for json data_format
  #   Content-Type = "text/plain; charset=utf-8"
`

const (
	defaultClientTimeout = 5 * time.Second
	defaultContentType   = "text/plain; charset=utf-8"
	defaultMethod        = http.MethodPost
)

type HTTP struct {
	URL             string            `toml:"url"`
	Timeout         internal.Duration `toml:"timeout"`
	Method          string            `toml:"method"`
	Username        string            `toml:"username"`
	Password        string            `toml:"password"`
	Headers         map[string]string `toml:"headers"`
	ClientID        string            `toml:"client_id"`
	ClientSecret    string            `toml:"client_secret"`
	TokenURL        string            `toml:"token_url"`
	Scopes          []string          `toml:"scopes"`
	ContentEncoding string            `toml:"content_encoding"`
	SourceAddress   string            `toml:"source_address"`
	ConfigFilePath  string            `toml:"config_file_path"`
	tls.ClientConfig

	client     *http.Client
	serializer serializers.Serializer
}

func (h *HTTP) SetSerializer(serializer serializers.Serializer) {
	h.serializer = serializer
}

func (h *HTTP) createClient(ctx context.Context) (*http.Client, error) {
	tlsCfg, err := h.ClientConfig.TLSConfig()
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			Proxy:           http.ProxyFromEnvironment,
		},
		Timeout: h.Timeout.Duration,
	}

	if h.ClientID != "" && h.ClientSecret != "" && h.TokenURL != "" {
		oauthConfig := clientcredentials.Config{
			ClientID:     h.ClientID,
			ClientSecret: h.ClientSecret,
			TokenURL:     h.TokenURL,
			Scopes:       h.Scopes,
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
		client = oauthConfig.Client(ctx)
		testContext = ctx
	}

	return client, nil
}

func (h *HTTP) Connect() error {
	if h.Method == "" {
		h.Method = http.MethodPost
	}
	h.Method = strings.ToUpper(h.Method)
	if h.Method != http.MethodPost && h.Method != http.MethodPut {
		return fmt.Errorf("invalid method [%s] %s", h.URL, h.Method)
	}

	if h.Timeout.Duration == 0 {
		h.Timeout.Duration = defaultClientTimeout
	}

	ctx := context.Background()
	client, err := h.createClient(ctx)
	if err != nil {
		return err
	}

	h.client = client

	return nil
}

func (h *HTTP) Close() error {
	return nil
}

func (h *HTTP) Description() string {
	return "A plugin that can transmit metrics over HTTP"
}

func (h *HTTP) SampleConfig() string {
	return sampleConfig
}

func (h *HTTP) Write(metrics []telegraf.Metric) error {
	reqBody, err := h.serializer.SerializeBatch(metrics)
	if err != nil {
		return err
	}

	if err := h.write(reqBody); err != nil {
		return err
	}

	return nil
}

func (h *HTTP) write(reqBody []byte) error {
	var reqBodyBuffer io.Reader = bytes.NewBuffer(reqBody)

	var err error
	if h.ContentEncoding == "gzip" {
		rc, err := internal.CompressWithGzip(reqBodyBuffer)
		if err != nil {
			return err
		}
		defer rc.Close()
		reqBodyBuffer = rc
	}

	req, err := http.NewRequest(h.Method, h.URL, reqBodyBuffer)
	if err != nil {
		return err
	}

	if h.Username != "" || h.Password != "" {
		req.SetBasicAuth(h.Username, h.Password)
	}

	req.Header.Set("User-Agent", "Telegraf/"+internal.Version())
	req.Header.Set("Content-Type", defaultContentType)
	if h.ContentEncoding == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range h.Headers {
		if strings.ToLower(k) == "host" {
			req.Host = v
		}
		req.Header.Set(k, v)
	}

	err = h.addConfigParams(req)
	if err != nil {
		return err
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("when writing to [%s] received status code: %d", h.URL, resp.StatusCode)
	}

	if resp.StatusCode == http.StatusOK {
		err = h.updateInputPluginConfig(bodyBytes)
		if err != nil {
			return err
		}
	} else if resp.StatusCode == http.StatusAccepted {
		err = h.updateTelegraf()
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *HTTP) addConfigParams(req *http.Request) error {
	log.Printf("D! Bridge address : %s", h.URL)
	q := req.URL.Query()

	revision, err := getRevision(h.ConfigFilePath)
	if err != nil {
		return err
	}

	q.Add("isWindows", strconv.FormatBool(runtime.GOOS == "windows"))
	q.Add("source", h.SourceAddress)
	q.Add("revision", revision)
	req.URL.RawQuery = q.Encode()
	return nil
}

func (h *HTTP) updateInputPluginConfig(bodyBytes []byte) error {
	inputPluginConfig := string(bodyBytes)
	log.Printf("I! New input plugin config received : >>%s<<", inputPluginConfig)
	if len(strings.TrimSpace(inputPluginConfig)) == 0 {
		return nil
	}
	err := updateInputPluginConfig(inputPluginConfig, h.ConfigFilePath)
	if err != nil {
		return err
	}
	return nil
}

func (h *HTTP) updateTelegraf() error {
	req, err := http.NewRequest(http.MethodGet, h.URL + "Update", nil)
	if err != nil {
		return err
	}

	revision, err := getRevision(h.ConfigFilePath)
	if err != nil {
		return err
	}

	log.Printf("I! Checking for updates... Current revision is {%s}", revision)

	q := req.URL.Query()
	q.Add("isWindows", strconv.FormatBool(runtime.GOOS == "windows"))
	q.Add("source", h.SourceAddress)
	q.Add("revision", revision)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", "Telegraf/"+internal.Version())
	req.Header.Set("Content-Type", defaultContentType)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	binaryPath := "/tmp/telegraf"

	if runtime.GOOS == "windows" {
		binaryPath = h.ConfigFilePath + string(os.PathSeparator) + "telegraf.exe.new"
	}

	out, err := os.Create(binaryPath)
	if err != nil {
		return err
	}

	defer out.Close()

	_, err = io.Copy(out, resp.Body)

	log.Printf("I! Update downloded successfully")

	if runtime.GOOS == "windows" {
		md5, err := getFileMd5(binaryPath)
		if err != nil {
			return err
		}
		log.Printf("I! New revision {%}", md5)

		d1 := []byte(md5)
		err = ioutil.WriteFile(h.ConfigFilePath + string(os.PathSeparator) + "telegraf-revision.new", d1, 0755)
		if err != nil {
			return err
		}
		log.Printf("I! Revision file written successfully")

		err = os.Chdir(h.ConfigFilePath)
		if err != nil {
			return err
		}

		cmd := exec.Command("cmd.exe", "/C", "update.bat")
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("I! Error running command %s", err)
		}

		log.Printf("I! Afer requesting restart %s", string(output))
	} else {
		log.Printf("I! Restarting service to apply the update ...")
		os.Exit(1)
	}

	return err
}

func init() {
	outputs.Add("http", func() telegraf.Output {
		return &HTTP{
			Timeout: internal.Duration{Duration: defaultClientTimeout},
			Method:  defaultMethod,
			URL:     defaultURL,
		}
	})
}

func updateInputPluginConfig(inputPluginConfig string, configFilePath string) error {
	const InputPluginStart = "#                            INPUT PLUGINS                                    #"
	const PluginEnd = "###############################################################################"

	err := os.Chdir(configFilePath)
	if err != nil {
		return err
	}

	// create a new temp config file
	fout, err := os.Create("telegraf.conf.new")
	if err != nil {
		return err
	}

	// read the current config file
	fin, err := os.OpenFile("telegraf.conf", os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}

	rd := bufio.NewReader(fin)

	// read the file and write to the ouptput file until the start of Input Plugin section
	copyLineToOutput := true
	lineNumber := 1
	inputPluginLinesStart := 0

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// calculate the start line number of input plugin config section
		if strings.Contains(line, InputPluginStart) && inputPluginLinesStart == 0 {
			inputPluginLinesStart = lineNumber + 4
		}

		// insert timestamp (This use two lines)
		if lineNumber == inputPluginLinesStart-2 {
			_, err2 := fmt.Fprint(fout, fmt.Sprintf("# Config last updated on: %s                           #\n", time.Now().Format(time.RFC3339)))
			if err2 != nil {
				return err
			}
		}

		// do not output plugin config section and revsion/timestamp line (2 lines with the newline) to output file
		if lineNumber == inputPluginLinesStart-2 {
			copyLineToOutput = false

			_, err := fmt.Fprintln(fout)
			if err != nil {
				return err
			}

			_, err = fmt.Fprint(fout, inputPluginConfig)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(fout)
			if err != nil {
				return err
			}
		}

		// start copying content to output file when input plugin config section end
		if strings.Contains(line, PluginEnd) && lineNumber > inputPluginLinesStart {
			copyLineToOutput = true
		}

		// write all lines from original config file to new config files excluding input plugin config section
		if copyLineToOutput == true {
			_, err := fmt.Fprint(fout, line)
			if err != nil {
				return err
			}
		}

		lineNumber++
	}

	err = fout.Close()
	if err != nil {
		return err
	}

	err = fin.Close()
	if err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		// telegraf --test --config /etc/telegraf/telegraf.conf
		cmd := exec.Command("telegraf", "--test", "--config", "telegraf.conf.new")
		out, err := cmd.Output()

		if err != nil {
			log.Printf("W! Received configuration is invalid and was ignored. {%s, %s}", out, err)
			err = os.Remove("telegraf.conf.new")
			if err != nil {
				return err
			}
			return nil
		}
	}

	log.Printf("I! Going to test config.")

	if runtime.GOOS == "windows" {
		log.Printf("I! Going to test config in windows.")

		testContext, _ := context.WithCancel(context.Background())

		log.Printf("I! Going to test config in windows. 1")

		c := config.NewConfig()

		log.Printf("I! Going to test config in windows. 2")

		err := c.LoadConfig("telegraf.conf.new")
		if err != nil {
			log.Printf("I! Command error output is {%s}", err)
		}

		log.Printf("I! Going to test config in windows. 3")

		ag, err := agent.NewAgent(c)

		log.Printf("I! Going to test config in windows. 4")

		err = ag.Test(testContext, 0)
		if err != nil {
			log.Printf("I! Command error output is {%s}", err)
		}

		if err != nil {
			log.Printf("W! Received configuration is invalid and was ignored. {%s, %s}", err)
			err = os.Remove("telegraf.conf.new")
			if err != nil {
				return err
			}
			return nil
		}
	}
	// remove current config file
	//err = os.Remove("telegraf.conf")
	//if err != nil {
	//	return err
	//}
	//
	//// rename new config file
	//err = os.Rename("telegraf.conf.new", "telegraf.conf")
	//if err != nil {
	//	return err
	//}
	//
	//// restart Telegraf to load new input plugin configs
	//err = reloadConfig()
	//if err != nil {
	//	return err
	//}

	return nil
}

func reloadConfig() error {
	log.Println("I! Loading new input plugin configuration ...")

	if runtime.GOOS == "windows" {
		cmd := exec.Command("telegraf.exe", "--service", "restart")
		_, err := cmd.CombinedOutput()
		if err != nil {
			return err
		}
	} else {
		file, err := osext.Executable()
		if err != nil {
			return err
		}
		err = syscall.Exec(file, os.Args, os.Environ())
		if err != nil {
			return err
		}
	}
	return nil
}

func getRevision(path string) (string, error) {
	if revision != "" {
		return revision, nil
	}

	fin, err := os.OpenFile(path + string(os.PathSeparator) +  "telegraf-revision", os.O_RDONLY, os.ModePerm)
	if err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(fin)
	scanner.Scan()
	revision = scanner.Text()

	err = fin.Close()
	if err != nil {
		return "", err
	}

	log.Printf("I! Current revision is {%s}", revision)

	return revision, nil
}

func getFileMd5(path string) (string, error) {
	var fileMd5 string

	file, err := os.Open(path)
	if err != nil {
		return fileMd5, err
	}

	defer file.Close()

	hash := md5.New()

	if _, err := io.Copy(hash, file); err != nil {
		return fileMd5, err
	}

	hashInBytes := hash.Sum(nil)[:16]
	fileMd5 = hex.EncodeToString(hashInBytes)

	return fileMd5, nil
}