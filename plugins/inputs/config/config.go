package config

import (
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"io"
	"net/http"
	"os"
	"time"
)

func check(e error) {
	if e != nil {
		panic(e)
	}
}

type Config struct {
	BridgeAddress string `toml:"bridge_address"`
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
	now := time.Now()

	resp, err := http.Get("http://localhost/bridge/telegraf")
	if err != nil {
		check(err)
	}

	defer resp.Body.Close()

	// Create the file
	out, err := os.Create("/Users/prasad/Desktop/telegrafconf_" + now.Format("20060102_150405") + ".conf")
	if err != nil {
		check(err)
	}

	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	check(err)

	return nil
}

func init() {
	inputs.Add("config", func() telegraf.Input {
		return &Config{}
	})
}
