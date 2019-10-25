package config

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func check(e error) {
	if e != nil {
		log.Print(e)
	}
}

type Config struct {
	BridgeAddress  string `toml:"bridge_address"`
	ConfigFilePath string `toml:"config_file_path"`
}

const sampleConfig = `
  ## Polling interval
  interval = "30s"
  ## Bridge address
  bridge_address = "influx"
`

// SampleConfig returns the default configuration of the Input
func (f *Config) SampleConfig() string {
	return sampleConfig
}

func (f *Config) Description() string {
	return "Poll the Bridge server for configuration changes and update the configuration."
}

func (f *Config) Gather(acc telegraf.Accumulator) error {

	log.Printf("Bridge address : %s", f.BridgeAddress)
	inputPluginConfigMd5 := calculateMd5OfInputPluginConfig(f.ConfigFilePath)

	//resp, err := http.Get("http://" + f.BridgeAddress + "/bridge/telegraf")
	//if err != nil {
	//	check(err)
	//}

	client := &http.Client{}
	req, err := http.NewRequest("GET", "http://"+f.BridgeAddress+"/bridge/telegraf", nil)
	if err != nil {
		check(err)
		return nil
	}

	println("input md5 : " + inputPluginConfigMd5)

	q := req.URL.Query()
	q.Add("md5", inputPluginConfigMd5)
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		check(err)
		return nil
	}

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			check(err)
			return nil
		}
		inputPluginConfig := string(bodyBytes)
		log.Printf("I! Input plugin config is \n%s\n", inputPluginConfig)
		log.Printf("I! Config file path : %s", f.ConfigFilePath)
		updateInputPluginConfig(inputPluginConfig, inputPluginConfigMd5, f.ConfigFilePath)
	}

	err1 := resp.Body.Close()
	if err1 != nil {
		check(err1)
		return nil
	}

	return nil
}

func init() {
	inputs.Add("config", func() telegraf.Input {
		return &Config{}
	})
}

func updateInputPluginConfig(inputPluginConfig string, inputPluginConfigMd5 string, configFilePath string) {
	const InputPluginStart = "#                            INPUT PLUGINS                                    #"
	const PluginEnd = "[[inputs.config]]"

	err := os.Chdir(configFilePath)
	if err != nil {
		check(err)
	}

	// create a new temp config file
	fout, err := os.Create("telegraf.conf.new")
	if err != nil {
		check(err)
	}

	// read the current config file
	fin, err := os.OpenFile("telegraf.conf", os.O_RDONLY, os.ModePerm)
	if err != nil {
		check(err)
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
			check(err)
			return
		}

		// calculate the start line number of input plugin config section
		if strings.Contains(line, InputPluginStart) {
			inputPluginLinesStart = lineNumber + 4
		}

		// insert revision (md5) and timestamp (This use two lines)
		if lineNumber == inputPluginLinesStart-2 {
			_, err2 := fmt.Fprint(fout, fmt.Sprintf("# Revision : %s, Timestamp : %s\n", inputPluginConfigMd5,
				time.Now().Format("2019-10-25 16:48:00 JST")))
			if err2 != nil {
				check(err2)
			}
		}

		// do not output plugin config section and revsion/timestamp line (2 lines with the newline) to output file
		if lineNumber == inputPluginLinesStart-2 {
			copyLineToOutput = false

			_, err1 := fmt.Fprintln(fout)
			if err1 != nil {
				check(err1)
			}

			_, err2 := fmt.Fprint(fout, inputPluginConfig)
			if err2 != nil {
				check(err2)
			}

			_, err3 := fmt.Fprintln(fout)
			if err3 != nil {
				check(err3)
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
				check(err)
			}
		}

		lineNumber++
	}

	err = fout.Close()
	if err != nil {
		check(err)
	}

	err = fin.Close()
	if err != nil {
		check(err)
	}

	// remove current config file
	err1 := os.Remove("telegraf.conf")
	if err1 != nil {
		check(err1)
	}

	// rename new config file
	err2 := os.Rename("telegraf.conf.new", "telegraf.conf")
	if err2 != nil {
		check(err2)
	}
}

func calculateMd5OfInputPluginConfig(configFilePath string) string {
	const InputPluginStart = "#                            INPUT PLUGINS                                    #"
	const PluginEnd = "[[inputs.config]]"

	err := os.Chdir(configFilePath)
	if err != nil {
		check(err)
	}

	// read the current config file
	fin, err := os.OpenFile("telegraf.conf", os.O_RDONLY, os.ModePerm)
	if err != nil {
		check(err)
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
			check(err)
			break
		}

		// calculate the start line number of input plugin config section
		if strings.Contains(line, InputPluginStart) {
			inputPluginLinesStart = lineNumber + 4
		}

		// write input plugin config section to the buffer
		if lineNumber == inputPluginLinesStart {
			writeToBuf = true
		}

		// break the loop after finish reading input plugin config
		if strings.Contains(line, PluginEnd) && lineNumber > inputPluginLinesStart {
			break
		}

		if writeToBuf && len(strings.TrimSpace(line)) > 0 {
			inputPluginConfigStr += line
			_, err := io.WriteString(inputPluginConfMd5, line)
			if err != nil {
				check(err)
			}
		}

		lineNumber++
	}

	err = fin.Close()
	if err != nil {
		check(err)
	}

	println("@@@" + inputPluginConfigStr + "@@@")
	return fmt.Sprintf("%x", inputPluginConfMd5.Sum(nil))
}
