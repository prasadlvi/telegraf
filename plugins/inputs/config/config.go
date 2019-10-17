package config

import (
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"io/ioutil"
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
	d1 := []byte("hello\ngo\n")
	now := time.Now()
	err := ioutil.WriteFile("/Users/prasad/Desktop/test_"+now.Format("20060102_150405")+".txt", d1, 0644)
	check(err)
	return nil
}

func init() {
	inputs.Add("config", func() telegraf.Input {
		return &Config{}
	})
}
