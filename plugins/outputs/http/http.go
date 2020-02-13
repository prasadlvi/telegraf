package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"github.com/kardianos/osext"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
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
		reqBodyBuffer, err = internal.CompressWithGzip(reqBodyBuffer)
		if err != nil {
			return err
		}
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

	inputPluginConfigMd5, err := calculateMd5OfInputPluginConfig(h.ConfigFilePath)
	err = h.addConfigParams(req, inputPluginConfigMd5)
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
		err = h.updateInputPluginConfig(bodyBytes, inputPluginConfigMd5)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *HTTP) addConfigParams(req *http.Request, inputPluginConfigMd5 string) error {
	log.Printf("Bridge address : %s", h.URL)
	q := req.URL.Query()
	q.Add("md5", inputPluginConfigMd5)
	q.Add("source", h.SourceAddress)
	req.URL.RawQuery = q.Encode()
	return nil
}

func (h *HTTP) updateInputPluginConfig(bodyBytes []byte, inputPluginConfigMd5 string) error {
	inputPluginConfig := string(bodyBytes)
	log.Printf("I! New input plugin config received : >>%s<<", inputPluginConfig)
	if len(strings.TrimSpace(inputPluginConfig)) == 0 {
		return nil
	}
	err := updateInputPluginConfig(inputPluginConfig, inputPluginConfigMd5, h.ConfigFilePath)
	if err != nil {
		return err
	}
	return nil
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

func updateInputPluginConfig(inputPluginConfig string, inputPluginConfigMd5 string, configFilePath string) error {
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

		// insert revision (md5) and timestamp (This use two lines)
		if lineNumber == inputPluginLinesStart-2 {
			_, err2 := fmt.Fprint(fout, fmt.Sprintf("# Revision: %s, Time: %s #\n", inputPluginConfigMd5,
				time.Now().Format(time.RFC3339)))
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

	// remove current config file
	err = os.Remove("telegraf.conf")
	if err != nil {
		return err
	}

	// rename new config file
	err = os.Rename("telegraf.conf.new", "telegraf.conf")
	if err != nil {
		return err
	}

	// restart Telegraf to load new input plugin configs
	err = reloadConfig()
	if err != nil {
		return err
	}

	return nil
}

func calculateMd5OfInputPluginConfig(configFilePath string) (string, error) {
	const InputPluginStart = "[[inputs."
	const PluginEnd = "###############################################################################"

	err := os.Chdir(configFilePath)
	if err != nil {
		return "", err
	}

	// read the current config file
	fin, err := os.OpenFile("telegraf.conf", os.O_RDONLY, os.ModePerm)
	if err != nil {
		return "", err
	}

	rd := bufio.NewReader(fin)

	writeToBuf := false
	lineNumber := 1
	inputPluginLinesStart := 0
	inputPluginConfMd5 := md5.New()

	inputPluginConfigStr := ""
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		// calculate the start line number of input plugin config section
		if strings.Contains(line, InputPluginStart) && inputPluginLinesStart == 0 {
			inputPluginLinesStart = lineNumber
		}

		// write input plugin config section to the buffer
		if lineNumber == inputPluginLinesStart {
			writeToBuf = true
		}

		// break the loop after finish reading input plugin config
		if strings.Contains(line, PluginEnd) && inputPluginLinesStart > 0 && lineNumber > inputPluginLinesStart {
			break
		}

		if writeToBuf && len(strings.TrimSpace(line)) > 0 {
			inputPluginConfigStr += line
		}

		lineNumber++
	}

	err = fin.Close()
	if err != nil {
		return "", err
	}

	inputPluginConfigStr = strings.TrimSuffix(strings.TrimSuffix(inputPluginConfigStr, "\n"), "\r")
	_, err = io.WriteString(inputPluginConfMd5, inputPluginConfigStr)

	inputPluginConfigStr = ">>" + inputPluginConfigStr + "<<"

	log.Printf("inputPluginConfMd5 : %s", inputPluginConfigStr)
	return fmt.Sprintf("%x", inputPluginConfMd5.Sum(nil)), nil
}

func reloadConfig() error {
	file, err := osext.Executable()
	if err != nil {
		return err
	}

	log.Println("Restarting Telegraf to load new input plugin configuration ...")
	err = syscall.Exec(file, os.Args, os.Environ())
	if err != nil {
		return err
	}
	return nil
}
