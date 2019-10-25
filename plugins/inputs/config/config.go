package config

import (
	"bufio"
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
		log.Fatal(e)
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

	resp, err := http.Get("http://" + f.BridgeAddress + "/bridge/telegraf")
	if err != nil {
		check(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			check(err)
		}
		inputPluginConfig := string(bodyBytes)
		log.Printf("I! Input plugin config is \n%s\n", inputPluginConfig)
		log.Printf("I! Config file path : %s", f.ConfigFilePath)
		updateInputPluginConfig(inputPluginConfig, f.ConfigFilePath)
	}

	return nil
}

func init() {
	inputs.Add("config", func() telegraf.Input {
		return &Config{}
	})
}

func updateInputPluginConfig(inputPluginConfig string, configFilePath string) {
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

	dir, err := os.Getwd()
	if err != nil {
		check(err)
	}

	println(dir)

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
			inputPluginLinesStart = lineNumber + 2
		}

		// do not output plugin config section to output file
		if lineNumber == inputPluginLinesStart {
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

	// rename file
	now := time.Now()
	err1 := os.Rename("telegraf.conf", "telegraf.conf."+now.Format("20060102_150405"))
	if err1 != nil {
		check(err1)
	}

	err2 := os.Rename("telegraf.conf.new", "telegraf.conf")
	if err2 != nil {
		check(err2)
	}
}

func calculateMd5OfInputPluginConfig() {

}
