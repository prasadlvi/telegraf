// +build !solaris

package tail

import (
	"bytes"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
	"io"
	"io/ioutil"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	ps "github.com/bhendo/go-powershell"
	"github.com/bhendo/go-powershell/backend"
	"github.com/influxdata/tail"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal/globpath"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers"
	"github.com/influxdata/telegraf/plugins/parsers/csv"
)

const (
	defaultWatchMethod = "inotify"
)

var (
	offsets      = make(map[string]int64)
	offsetsMutex = new(sync.Mutex)
)

type Tail struct {
	Files         []string
	FromBeginning bool
	Pipe          bool
	WatchMethod   string

	Log telegraf.Logger

	tailers    map[string]*tail.Tail
	offsets    map[string]int64
	parserFunc parsers.ParserFunc
	wg         sync.WaitGroup
	acc        telegraf.Accumulator
	isJIS	   bool

	sync.Mutex
}

func NewTail() *Tail {
	offsetsMutex.Lock()
	offsetsCopy := make(map[string]int64, len(offsets))
	for k, v := range offsets {
		offsetsCopy[k] = v
	}
	offsetsMutex.Unlock()

	return &Tail{
		FromBeginning: false,
		offsets:       offsetsCopy,
	}
}

const sampleConfig = `
  ## files to tail.
  ## These accept standard unix glob matching rules, but with the addition of
  ## ** as a "super asterisk". ie:
  ##   "/var/log/**.log"  -> recursively find all .log files in /var/log
  ##   "/var/log/*/*.log" -> find all .log files with a parent dir in /var/log
  ##   "/var/log/apache.log" -> just tail the apache log file
  ##
  ## See https://github.com/gobwas/glob for more examples
  ##
  files = ["/var/mymetrics.out"]
  ## Read file from beginning.
  from_beginning = false
  ## Whether file is a named pipe
  pipe = false

  ## Method used to watch for file updates.  Can be either "inotify" or "poll".
  # watch_method = "inotify"

  ## Data format to consume.
  ## Each data format has its own unique set of configuration options, read
  ## more about them here:
  ## https://github.com/influxdata/telegraf/blob/master/docs/DATA_FORMATS_INPUT.md
  data_format = "influx"
`

func (t *Tail) SampleConfig() string {
	return sampleConfig
}

func (t *Tail) Description() string {
	return "Stream a log file, like the tail -f command"
}

func (t *Tail) Gather(acc telegraf.Accumulator) error {
	t.Lock()
	defer t.Unlock()

	return t.tailNewFiles(true)
}

func (t *Tail) Start(acc telegraf.Accumulator) error {
	t.Lock()
	defer t.Unlock()

	t.acc = acc
	t.tailers = make(map[string]*tail.Tail)

	err := t.tailNewFiles(t.FromBeginning)

	// clear offsets
	t.offsets = make(map[string]int64)
	// assumption that once Start is called, all parallel plugins have already been initialized
	offsetsMutex.Lock()
	offsets = make(map[string]int64)
	offsetsMutex.Unlock()

	if runtime.GOOS == "windows" {
		back := &backend.Local{}
		shell, err := ps.New(back)
		if err != nil {
			t.Log.Warn("Error occurred", err)
		}
		defer shell.Exit()

		encoding, _, err := shell.Execute("[System.Text.Encoding]::Default.EncodingName")
		if err != nil {
			t.Log.Warn("Error occurred", err)
		}
		t.Log.Debug("PS Encoding: ", encoding)

		if strings.Contains(encoding, "JIS") {
			t.isJIS = true
		}
	}

	return err
}

func (t *Tail) tailNewFiles(fromBeginning bool) error {
	var poll bool
	if t.WatchMethod == "poll" {
		poll = true
	}

	// Create a "tailer" for each file
	for _, filepath := range t.Files {
		g, err := globpath.Compile(filepath)
		if err != nil {
			t.Log.Errorf("Glob %q failed to compile: %s", filepath, err.Error())
		}
		for _, file := range g.Match() {
			if _, ok := t.tailers[file]; ok {
				// we're already tailing this file
				continue
			}

			var seek *tail.SeekInfo
			if !t.Pipe && !fromBeginning {
				if offset, ok := t.offsets[file]; ok {
					t.Log.Debugf("Using offset %d for %q", offset, file)
					seek = &tail.SeekInfo{
						Whence: 0,
						Offset: offset,
					}
				} else {
					seek = &tail.SeekInfo{
						Whence: 2,
						Offset: 0,
					}
				}
			}

			tailer, err := tail.TailFile(file,
				tail.Config{
					ReOpen:    true,
					Follow:    true,
					Location:  seek,
					MustExist: true,
					Poll:      poll,
					Pipe:      t.Pipe,
					Logger:    tail.DiscardingLogger,
				})
			if err != nil {
				t.Log.Debugf("Failed to open file (%s): %v", file, err)
				continue
			}

			t.Log.Debugf("Tail added for %q", file)

			parser, err := t.parserFunc()
			if err != nil {
				t.Log.Errorf("Creating parser: %s", err.Error())
			}

			// create a goroutine for each "tailer"
			t.wg.Add(1)
			go func() {
				defer t.wg.Done()
				if parser.IsMultiline() {
					t.multilineReceiver(parser, tailer)
				} else {
					t.receiver(parser, tailer)
				}
			}()
			t.tailers[tailer.Filename] = tailer
		}
	}
	return nil
}

// ParseLine parses a line of text.
func parseLine(parser parsers.Parser, line string, firstLine bool) ([]telegraf.Metric, error) {
	switch parser.(type) {
	case *csv.Parser:
		// The csv parser parses headers in Parse and skips them in ParseLine.
		// As a temporary solution call Parse only when getting the first
		// line from the file.
		if firstLine {
			return parser.Parse([]byte(line))
		} else {
			m, err := parser.ParseLine(line)
			if err != nil {
				return nil, err
			}

			if m != nil {
				return []telegraf.Metric{m}, nil
			}
			return []telegraf.Metric{}, nil
		}
	default:
		return parser.Parse([]byte(line))
	}
}

// Receiver is launched as a goroutine to continuously watch a tailed logfile
// for changes, parse any incoming msgs, and add to the accumulator.
func (t *Tail) receiver(parser parsers.Parser, tailer *tail.Tail) {
	var firstLine = true
	for line := range tailer.Lines {
		if line.Err != nil {
			t.Log.Errorf("Tailing %q: %s", tailer.Filename, line.Err.Error())
			continue
		}
		// Fix up files with Windows line endings.
		text := strings.TrimRight(line.Text, "\r")

		if runtime.GOOS == "windows" {
			if t.isJIS {
				text, _ = FromShiftJIS(text)
			}
		}

		metrics, err := parseLine(parser, text, firstLine)
		if err != nil {
			t.Log.Errorf("Malformed log line in %q: [%q]: %s",
				tailer.Filename, line.Text, err.Error())
			continue
		}
		firstLine = false

		for _, metric := range metrics {
			metric.AddTag("path", tailer.Filename)
			t.acc.AddMetric(metric)
		}
	}

	t.Log.Debugf("Tail removed for %q", tailer.Filename)

	if err := tailer.Err(); err != nil {
		t.Log.Errorf("Tailing %q: %s", tailer.Filename, err.Error())
	}
}

// Multiline Receiver is launched if MULTILINE is enabled and run as a goroutine to continuously watch a tailed logfile
// for changes, parse any incoming msgs, and add to the accumulator.
func (t *Tail) multilineReceiver(parser parsers.Parser, tailer *tail.Tail) {
	var firstLine = true
	var buffer bytes.Buffer
	var bufferLastModifiedTime time.Time

	go bufferMonitor(&buffer, &bufferLastModifiedTime, parser, tailer, t)
	for line := range tailer.Lines {
		log.Printf("Processing log line %q", line.Text)
		if line.Err != nil {
			t.Log.Errorf("Tailing %q: %s", tailer.Filename, line.Err.Error())
			continue
		}
		// Fix up files with Windows line endings.
		text := strings.TrimRight(line.Text, "\r")

		if runtime.GOOS == "windows" {
			if t.isJIS {
				text, _ = FromShiftJIS(text)
			}
		}

		var startOfLogLine, err = parser.IsNewLogLine(text)
		if err != nil {
			t.Log.Errorf("Malformed log line in %q: [%q]: %s", tailer.Filename, text, err.Error())
		}

		if startOfLogLine {
			t.Log.Debugf("Start of new line detected")

			if buffer.Len() > 0 {
				var multilineLogLine = buffer.String()
				t.Log.Debugf("Multiline log line in a single line %q", multilineLogLine)
				metrics, err := parseLine(parser, multilineLogLine, firstLine)
				if err != nil {
					t.Log.Errorf("Malformed log line in %q: [%q]: %s",
						tailer.Filename, multilineLogLine, err.Error())
					continue
				}
				firstLine = false

				for _, metric := range metrics {
					metric.AddTag("path", tailer.Filename)
					t.acc.AddMetric(metric)
				}
			}

			t.Log.Debugf("Resetting the buffer. Starting reading a new line.")
			buffer.Reset()
			buffer.WriteString(text)

		} else {
			buffer.WriteString(" ")
			buffer.WriteString(text)
			bufferLastModifiedTime = time.Now()
		}

	}

	t.Log.Debugf("Tail removed for %q", tailer.Filename)

	if err := tailer.Err(); err != nil {
		t.Log.Errorf("Tailing %q: %s", tailer.Filename, err.Error())
	}
}

func bufferMonitor(buf *bytes.Buffer, bufferLastModifiedTime *time.Time, parser parsers.Parser, tailer *tail.Tail, t *Tail) {
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ticker.C:
			buffer := *buf
			now := time.Now()
			if now.Sub(*bufferLastModifiedTime).Seconds() > 1 {
				if buffer.Len() > 0 {
					var multilineLogLine = buffer.String()
					t.Log.Debugf("Multiline log line in a single line %q", multilineLogLine)
					metrics, err := parseLine(parser, multilineLogLine, false)
					if err != nil {
						t.Log.Errorf("Malformed log line in %q: [%q]: %s", tailer.Filename, multilineLogLine, err.Error())
						continue
					}

					for _, metric := range metrics {
						metric.AddTag("path", tailer.Filename)
						t.acc.AddMetric(metric)
					}

					t.Log.Debugf("Resetting the buffer.")
					(*buf).Reset()
				}
			}
		}
	}
}

func (t *Tail) Stop() {
	t.Lock()
	defer t.Unlock()

	for _, tailer := range t.tailers {
		if !t.Pipe && !t.FromBeginning {
			// store offset for resume
			offset, err := tailer.Tell()
			if err == nil {
				t.Log.Debugf("Recording offset %d for %q", offset, tailer.Filename)
			} else {
				t.Log.Errorf("Recording offset for %q: %s", tailer.Filename, err.Error())
			}
		}
		err := tailer.Stop()
		if err != nil {
			t.Log.Errorf("Stopping tail on %q: %s", tailer.Filename, err.Error())
		}
	}

	t.wg.Wait()

	// persist offsets
	offsetsMutex.Lock()
	for k, v := range t.offsets {
		offsets[k] = v
	}
	offsetsMutex.Unlock()
}

func (t *Tail) SetParserFunc(fn parsers.ParserFunc) {
	t.parserFunc = fn
}

func init() {
	inputs.Add("tail", func() telegraf.Input {
		return NewTail()
	})
}

func FromShiftJIS(str string) (string, error) {
	return transformEncoding(strings.NewReader(str), japanese.ShiftJIS.NewDecoder())
}

func transformEncoding(rawReader io.Reader, trans transform.Transformer) (string, error) {
	ret, err := ioutil.ReadAll(transform.NewReader(rawReader, trans))
	if err == nil {
		return string(ret), nil
	} else {
		return "", err
	}
}