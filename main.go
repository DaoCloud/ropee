package main

import (
	"flag"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/kebe7jun/ropee/metrics"
	"github.com/kebe7jun/ropee/storage"
	"github.com/lestrrat/go-file-rotatelogs"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/prometheus/prompb"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"time"
)

type Config struct {
	SplunkUrl               string
	SplunkMetricsIndex      string
	SplunkMetricsSourceType string
	SplunkHECURL            string
	SplunkHECToken          string
	TimeoutSeconds          int
	ListenAddr              string
	LogFilePath             string
	Debug                   bool
}

var config Config

func loadRotateWriter(logPath, fileName string) *rotatelogs.RotateLogs {
	writer, _ := rotatelogs.New(
		path.Join(logPath, fileName)+".%Y%m%d%H%M",
		rotatelogs.WithLinkName(path.Join(logPath, fileName)), // 生成软链，指向最新日志文件
		rotatelogs.WithMaxAge(7*24*time.Hour),                 // 文件最大保存时间
		rotatelogs.WithRotationTime(48*time.Hour),             // 日志切割时间间隔
	)
	return writer
}

func loadLogger() log.Logger {
	var logger log.Logger
	if config.LogFilePath == "-" {
		logger = log.NewLogfmtLogger(os.Stdout)
	} else {
		logger = log.NewLogfmtLogger(log.NewSyncWriter(loadRotateWriter(config.LogFilePath, "ropee.log")))
	}

	if config.Debug {
		logger = level.NewFilter(logger, level.AllowDebug())
	} else {
		logger = level.NewFilter(logger, level.AllowInfo())
	}
	logger = log.With(logger, "time", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	return logger
}

func init() {
	// init config
	flag.StringVar(&config.SplunkUrl, "splunk-url", "https://127.0.0.1:8089", "Splunk Manage Url.")
	flag.StringVar(&config.SplunkHECURL, "splunk-hec-url", "https://127.0.0.1:8088", "Splunk Http event collector url.")
	flag.StringVar(&config.SplunkHECToken, "splunk-hec-token", "", "Splunk Http event collector token.")
	flag.StringVar(&config.ListenAddr, "listen-addr", "127.0.0.1:9970", "Sopee listen addr.")
	flag.StringVar(&config.SplunkMetricsIndex, "splunk-metrics-index", "*", "Index name.")
	flag.StringVar(&config.SplunkMetricsSourceType, "splunk-metrics-sourcetype", "DaoCloud_promu_metrics", "The prometheus sourcetype name.")
	flag.StringVar(&config.LogFilePath, "log-file-path", "/var/log", "Log files path.")
	flag.IntVar(&config.TimeoutSeconds, "timeout", 60, "API timeout seconds.")
	flag.BoolVar(&config.Debug, "debug", false, "Debug mode.")
	flag.Parse()
}

func main() {
	l := loadLogger()
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			level.Error(l).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			level.Error(l).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		metrics.ReadRequestCounter.Add(1)
		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			level.Error(l).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, pass, _ := r.BasicAuth()
		readClient, _ := storage.NewClient(
			config.SplunkUrl,
			user,
			pass,
			config.SplunkMetricsIndex,
			config.SplunkMetricsSourceType,
			config.SplunkHECURL, config.SplunkHECToken,
			time.Second*time.Duration(config.TimeoutSeconds),
			l,
		)
		resp, err := readClient.Read(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "snappy")

		compressed = snappy.Encode(nil, data)
		if _, err := w.Write(compressed); err != nil {
			level.Warn(l).Log("msg", "Error executing query", "query", req, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	writeClient, _ := storage.NewClient(
		config.SplunkUrl,
		"",
		"",
		config.SplunkMetricsIndex,
		config.SplunkMetricsSourceType,
		config.SplunkHECURL, config.SplunkHECToken,
		time.Second*time.Duration(config.TimeoutSeconds),
		l,
	)
	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			level.Error(l).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			level.Error(l).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		metrics.WriteRequestCounter.Add(1)
		var req prompb.WriteRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			level.Error(l).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = writeClient.Write(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(200)
		if _, err := w.Write([]byte("ok")); err != nil {
			level.Error(l).Log("action", "write", "err", err)
		}
	})
	level.Info(l).Log("msg", "starting server...", "listen", config.ListenAddr)
	if err := http.ListenAndServe(config.ListenAddr, nil); err != nil {
		level.Error(l).Log("action", "serve", "err", err)
	}
}
