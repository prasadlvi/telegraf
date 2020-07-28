package config

import (
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"log"
	"time"
)

type Config struct {}

func (f *Config) SampleConfig() string {
	return ""
}

func (f *Config) Description() string {
	return "Collect dummy metrics to send to the ThirdEye server to trigger config update."
}

func (f *Config) Gather(acc telegraf.Accumulator) error {
	log.Printf("D! Running config input plugin")

	now := time.Now()

	fields := map[string]interface{}{
		"dummy_field":       "dummyValue",
	}

	acc.AddSummary("config", fields, nil, now)

	return nil
}

func init() {
	inputs.Add("config", func() telegraf.Input {
		return &Config{}
	})
}
