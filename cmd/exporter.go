package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sap/gorfc/gorfc"
	log "github.com/sirupsen/logrus"
	"github.com/ulranh/sapnwrfc_exporter/internal"
)

type collector struct {
	// possible metric descriptions.
	Desc *prometheus.Desc

	// a parameterized function used to gather metrics.
	stats func() []metricData
}

type metricData struct {
	name       string
	help       string
	metricType string
	stats      []statData
}

type statData struct {
	value       float64
	labels      []string
	labelValues []string
}

// start collector and web server
func (config *Config) web(flags map[string]*string) error {

	// append missing system data
	err := config.appendMissingData()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("Can't add missing config data.")
		return err
	}

	stats := func() []metricData {
		data := config.collectMetrics()
		return data
	}

	c := newCollector(stats)
	prometheus.MustRegister(c)

	// start http server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:         ":" + *flags["port"],
		Handler:      mux,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	}
	err = server.ListenAndServe()
	if err != nil {
		return errors.Wrap(err, " web - ListenAndServe")
	}

	return nil
}

// append system password and system servers to config.Systems
func (config *Config) appendMissingData() error {
	var secret internal.Secret
	if err := proto.Unmarshal(config.Secret, &secret); err != nil {
		log.Fatal("Secret Values don't exist or are corrupted")
		return errors.Wrap(err, " system  - Unmarshal")
	}

	for _, system := range config.Systems {

		// decrypt password and add it to system config
		if _, ok := secret.Name[system.Name]; !ok {
			log.WithFields(log.Fields{
				"system": system.Name,
			}).Error("Can't find password for system")
			continue
		}
		pw, err := internal.PwDecrypt(secret.Name[system.Name], secret.Name["secretkey"])
		if err != nil {
			log.WithFields(log.Fields{
				"system": system.Name,
			}).Error("Can't decrypt password for system")
			continue
		}
		system.password = pw

		// retrieve system servers and add them to config
		c, err := connect(system, serverInfo{system.Server, system.Sysnr})
		if err != nil {
			continue
		}
		defer c.Close()

		params := map[string]interface{}{}
		r, err := c.Call("TH_SERVER_LIST", params)
		if err != nil {
			log.WithFields(log.Fields{
				"system": system.Name,
				"error":  err,
			}).Error("Can't call fumo th_server_list")
			continue
		}

		for _, v := range r["LIST"].([]interface{}) {
			appl := v.(map[string]interface{})
			info := strings.Split(strings.TrimSpace(appl["NAME"].(string)), "_")
			server := serverInfo{
				name:  strings.TrimSpace(info[0]),
				sysnr: strings.TrimSpace(info[2]),
			}
			system.servers = append(system.servers, server)

		}
	}
	return nil
}

// start collecting all metrics and fetch the results
func (config *Config) collectMetrics() []metricData {

	// start := time.Now()
	// log.WithFields(log.Fields{
	// 	"timestamp": start,
	// }).Info("Start scraping")

	resC := make(chan metricData)
	go func(metrics []*metricInfo, systems []*systemInfo) {
		var wg sync.WaitGroup

		for _, metric := range metrics {

			wg.Add(1)
			go func(metric *metricInfo, systems []*systemInfo) {
				defer wg.Done()
				resC <- metricData{
					name:       metric.Name,
					help:       metric.Help,
					metricType: metric.MetricType,
					stats:      collectSystemsMetric(metric, systems),
				}
			}(metric, systems)
		}
		wg.Wait()
		close(resC)
	}(config.TableMetrics, config.Systems)

	var metrics []metricData
	for metric := range resC {
		metrics = append(metrics, metric)
	}

	// log.WithFields(log.Fields{
	// 	"timestamp": time.Since(start),
	// }).Info("Finish scraping")
	return metrics
}

// start collecting metric information for all tenants
func collectSystemsMetric(metric *metricInfo, systems []*systemInfo) []statData {
	resC := make(chan []statData)

	go func(metric *metricInfo, systems []*systemInfo) {
		var wg sync.WaitGroup

		for _, system := range systems {

			wg.Add(1)
			go func(metric *metricInfo, system *systemInfo) {
				defer wg.Done()

				resC <- getMetricSystemData(metric, system)
			}(metric, system)
		}
		wg.Wait()
		close(resC)
	}(metric, systems)

	var statData []statData
	for v := range resC {
		if v != nil {
			statData = append(statData, v...)
		}
	}
	return statData
}

// get metric data for all systems application servers
func getMetricSystemData(metric *metricInfo, system *systemInfo) []statData {

	resC := make(chan []statData)
	go func(metric *metricInfo, system *systemInfo) {
		var wg sync.WaitGroup

		for _, server := range system.servers {

			wg.Add(1)
			go func(metric *metricInfo, system *systemInfo, server serverInfo) {
				defer wg.Done()
				resC <- getRfcData(metric, system, server)
			}(metric, system, server)

			// stop if fumo must be called only once
			if !metric.AllServers {
				break
			}
		}
		wg.Wait()
		close(resC)
	}(metric, system)

	var statData []statData
	for v := range resC {
		if v != nil {
			statData = append(statData, v...)
		}
	}
	return statData
}

type rfcData map[string]interface{}

// get rfc data from sap system
func getRfcData(metric *metricInfo, system *systemInfo, server serverInfo) []statData {

	// connect to system/server
	c, err := connect(system, server)
	if err != nil {
		return nil
	}
	defer c.Close()

	// all values of Metrics.TagFilter must be in Tenants.Tags, otherwise the
	// metric is not relevant for the tenant
	if !subSliceInSlice(metric.TagFilter, system.Tags) {
		return nil
	}

	// call metrics function module
	var res rfcData
	res, err = c.Call(metric.FuMo, metric.Params)
	if err != nil {
		log.WithFields(log.Fields{
			"system": system.Name,
			"server": server.name,
			"error":  err,
		}).Error("Can't call function module")
		return nil
	}

	return res.collectTableData(metric, system, server)
}

// get table information - occurrences of specified table field values
func (tableData rfcData) collectTableData(metric *metricInfo, system *systemInfo, server serverInfo) []statData {

	var md []statData
	count := make(map[string]float64)

	for _, res := range tableData[metric.Table].([]interface{}) {
		line := res.(map[string]interface{})

		if len(metric.RowFilter) == 0 || inFilter(line, metric.RowFilter) {
			for field, values := range metric.RowCount {
				for _, value := range values {
					namePart := interface2String(value)
					if "" == namePart {
						log.WithFields(log.Fields{
							"metric": metric.Name,
							"system": system.Name,
						}).Error("Configfile RowCount: only string and int types are allowed")
						continue
					}

					if strings.HasPrefix(strings.ToLower(interface2String(line[strings.ToUpper(field)])), strings.ToLower(namePart)) || strings.EqualFold("total", namePart) {
						count[field+"_"+namePart]++
					}
				}
			}
		}
	}

	for field, values := range metric.RowCount {
		for _, value := range values {
			namePart := interface2String(value)

			data := statData{
				labels:      []string{"system", "usage", "server", "count"},
				labelValues: []string{strings.ToLower(system.Name), strings.ToLower(system.Usage), strings.ToLower(server.name), strings.ToLower(field + "_" + namePart)},
				value:       count[field+"_"+namePart],
			}
			md = append(md, data)
		}
	}

	return md
}

func inFilter(line map[string]interface{}, filter map[string][]interface{}) bool {
	for field, values := range filter {
		for _, value := range values {
			if strings.EqualFold(interface2String(line[strings.ToUpper(field)]), interface2String(value)) {
				return true
			}
		}
	}
	return false

}

// convert interface int values to string
func interface2String(namePart interface{}) string {

	switch val := namePart.(type) {
	case string:
		return val
	case int64, int32, int16, int8, int, uint64, uint32, uint8, uint:
		// return strconv.FormatInt(val, 10)
		return fmt.Sprint(val)
	default:
		return ""
	}
}

func newCollector(stats func() []metricData) *collector {
	return &collector{
		stats: stats,
	}
}

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// Take a stats snapshot.  Must be concurrency safe.
	stats := c.stats()

	var valueType = map[string]prometheus.ValueType{
		"gauge":   prometheus.GaugeValue,
		"counter": prometheus.CounterValue,
	}
	for _, mi := range stats {
		for _, v := range mi.stats {
			m := prometheus.MustNewConstMetric(
				prometheus.NewDesc(strings.ToLower(mi.name), mi.help, v.labels, nil),
				valueType[strings.ToLower(mi.metricType)],
				v.value,
				v.labelValues...,
			)
			ch <- m
		}
	}
}

// connect to sap system
func connect(system *systemInfo, server serverInfo) (*gorfc.Connection, error) {
	c, err := gorfc.ConnectionFromParams(
		gorfc.ConnectionParameter{
			Dest:   system.Name,
			User:   system.User,
			Passwd: system.password,
			Client: system.Client,
			Lang:   system.Lang,
			// Lang:   "en",
			Ashost: server.name,
			Sysnr:  server.sysnr,
			// Ashost: config.Systems[s].Server,
			// Sysnr:  config.Systems[s].Sysnr,
			// Saprouter: "/H/203.13.155.17/S/3299/W/xjkb3d/H/172.19.137.194/H/",
		},
	)
	if err != nil {
		log.WithFields(log.Fields{
			"system": system.Name,
			"server": server.name,
			"error":  err,
		}).Error("Can't connect to system with user/password")
		return nil, err
	}

	return c, nil
}

// true if every item in sublice exists in slice
func subSliceInSlice(subSlice []string, slice []string) bool {
	for _, vs := range subSlice {
		for _, v := range slice {
			if strings.EqualFold(vs, v) {
				goto nextCheck
			}
		}
		return false
	nextCheck:
	}
	return true
}
