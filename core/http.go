package core

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"pilosa/config"
	"pilosa/db"
	"pilosa/index"
	"pilosa/util"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"time"

	notify "github.com/bitly/go-notify"
	"github.com/davecgh/go-spew/spew"
	"github.com/gorilla/websocket"
)

type WebService struct {
	service *Service
	end     chan bool
}

func NewWebService(service *Service) *WebService {
	e := make(chan bool)
	return &WebService{service, e}
}

type Flusher struct {
	flusher chan bool
}

func (self *Flusher) Handle(w http.ResponseWriter, r *http.Request) {
	self.flusher <- true
}

func NewFlusher(flusher chan bool) http.HandlerFunc {
	f := Flusher{flusher}
	return f.Handle
}

type RequestLogger struct {
	handler http.HandlerFunc
	logger  chan []byte
}

func NewRequestLogger(handler http.HandlerFunc, logger chan []byte) http.HandlerFunc {
	r := RequestLogger{handler, logger}
	return r.Handle
}

func (self *RequestLogger) Handle(w http.ResponseWriter, r *http.Request) {

	// Grab a dump of the incoming request
	dump, err := httputil.DumpRequest(r, true /*dump the body also*/)
	if err != nil {
		log.Println("Dump Failure", err)
	}

	self.handler(w, r)
	self.logger <- dump
}

type LogRecord struct {
	When     time.Time
	Data_x64 string
}

func NewLogRecord(t time.Time, data []byte) LogRecord {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Flush()
	w.Close()
	e := base64.StdEncoding.EncodeToString(b.Bytes())
	x := LogRecord{t, e}
	return x
}
func genFileName(id string) string {
	//bucket/YYYY/MM/DDHHMMSS.id.log
	t := time.Now()
	//base := "http://pilosa.umbel.com.s3.amazonaws.com/bit_log"
	base := config.GetStringDefault("pilosa_request_log", "/tmp/set_bit_log")
	return fmt.Sprintf("%s%s.%s.log", base, t.Format("/2006/01/02/15/04-05"), id)

}

func flush(requests []LogRecord, id string, records_to_dump int) {
	dest := genFileName(id)
	w, err := util.Create(dest)
	if err != nil {
		log.Println("Error opening outfile ", dest)
		log.Println(err)
		return
	}
	defer w.Close()

	encoder := json.NewEncoder(w)
	for i := 0; i < records_to_dump; i++ {
		encoder.Encode(requests[i])
	}

}

func Logger(in chan []byte, end chan bool, id string, flusher chan bool) {
	var buffer = make([]LogRecord, 2048, 2048)
	i := 0
	for {
		select {
		case raw := <-in:
			logRecord := NewLogRecord(time.Now(), raw)
			buffer[i] = logRecord
			i += 1
			if i > 2047 {
				flush(buffer, id, i)
				i = 0
			}

		case <-flusher:
			if i > 0 {
				flush(buffer, id, i)
				i = 0
			}
		case <-end:
			flush(buffer, id, i)
			log.Println("Shutdown Logger")
			return
		}

	}

}

func (self *WebService) Run() {
	port_string := strconv.Itoa(config.GetInt("port_http"))
	log.Printf("Serving HTTP on port %s...\n", port_string)
	logger_chan := make(chan []byte, 1024)
	flusher := make(chan bool)
	mux := http.NewServeMux()
	mux.HandleFunc("/message", self.HandleMessage)
	mux.HandleFunc("/query", self.HandleQuery)
	mux.HandleFunc("/stats", self.HandleStats)
	mux.HandleFunc("/status", self.HandleStatus)
	mux.HandleFunc("/info", self.HandleInfo)
	mux.HandleFunc("/processes", self.HandleProcesses)
	mux.HandleFunc("/listen/ws", self.HandleListenWS)
	mux.HandleFunc("/listen/stream", self.HandleListenStream)
	mux.HandleFunc("/listen", self.HandleListen)
	mux.HandleFunc("/test", self.HandleTest)
	mux.HandleFunc("/version", self.HandleVersion)
	mux.HandleFunc("/ping", self.HandlePing)
	mux.HandleFunc("/batch", self.HandleBatch)
	log_set_bit := config.GetIntDefault("log_set_bit_request", 0)
	if log_set_bit == 1 {
		mux.HandleFunc("/set_bits", NewRequestLogger(self.HandleSetBit, logger_chan))
	} else {
		mux.HandleFunc("/set_bits", self.HandleSetBit)
	}
	//mux.HandleFunc("/set_bits", self.HandleSetBit)
	mux.HandleFunc("/flush", NewFlusher(flusher))
	s := &http.Server{
		Addr:    ":" + port_string,
		Handler: mux,
	}
	id := config.GetString("id")
	go Logger(logger_chan, self.end, id, flusher)
	s.ListenAndServe()
}
func (self *WebService) Shutdown() {
	self.end <- true

}

func (self *WebService) HandleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}
	var message db.Message
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(&message) != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	//service.Inbox <- &message
}

func (self *WebService) HandleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}
	/*   	values.Set("db", database)
	values.Set("id", id)
	values.Set("frame", fragment_type)
	values.Set("slice", fmt.Sprintf("%d", slice))
	values.Set("bitmap", compressed_string)
	*/

	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	database_name := r.Form.Get("db")
	if database_name == "" {
		http.Error(w, "Provide a database (db)", http.StatusNotFound)
		return
	}

	bitmap_id_string := r.Form.Get("id")
	if bitmap_id_string == "" {
		http.Error(w, "Provide a bitmap id (id)", http.StatusNotFound)
		return
	}
	bitmap_id, _ := strconv.ParseUint(bitmap_id_string, 10, 64)

	slice_string := r.Form.Get("slice")
	if bitmap_id_string == "" {
		http.Error(w, "Provide a slice (slice)", http.StatusNotFound)
		return
	}
	slice, _ := strconv.ParseInt(slice_string, 10, 32)

	frame := r.Form.Get("frame")
	if bitmap_id_string == "" {
		http.Error(w, "Provide a frame (frame)", http.StatusNotFound)
		return
	}

	compressed_bitmap := r.Form.Get("bitmap")
	if bitmap_id_string == "" {
		http.Error(w, "Provide a compressed base64 bitmap (bitmap)", http.StatusNotFound)
		return
	}

	filter_string := r.Form.Get("filter")
	filter, _ := strconv.ParseInt(filter_string, 10, 32)
	if bitmap_id_string == "" {
		http.Error(w, "Provide a filter for categories", http.StatusNotFound)
		return
	}

	results := self.service.Batch(database_name, frame, compressed_bitmap, bitmap_id, int(slice), uint64(filter))

	encoder := json.NewEncoder(w)
	err = encoder.Encode(results)
	if err != nil {
		log.Println("Error Batch results")
		log.Println(spew.Sdump(r.Form))
		err = encoder.Encode("Bad Batch Request")
	}

}

func (self *WebService) HandleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}
	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	database_name := r.Form.Get("db")
	if database_name == "" {
		database_name = config.GetString("default_db")
	}
	if database_name == "" {
		http.Error(w, "Provide a database (db)", http.StatusNotFound)
		return
	}
	pql := r.Form.Get("pql")
	if pql == "" {
		http.Error(w, "Provide a valid query string (pql)", http.StatusNotFound)
		return
	}
	_, bits := r.Form["bits"]

	results, err := self.service.Executor.RunPQL(database_name, pql)
	switch r := results.(type) { //a hack to handle empty sets
	case []index.Pair:
		if len(r) == 0 {
			results = []int{}

		}
	case []byte: //can i figure out the type of the compressed string here?
		if bits {
			reader, _ := gzip.NewReader(bytes.NewReader(r))
			b, _ := ioutil.ReadAll(reader)
			result := index.NewBitmap()
			result.FromBytes(b)
			results = result.Bits()
		}

	}

	if results == nil {
		log.Println("Empty results")
	}
	if err != nil {
		http.Error(w, "Error running query: "+err.Error(), http.StatusInternalServerError)
	}

	encoder := json.NewEncoder(w)
	err = encoder.Encode(results)
	if err != nil {
		http.Error(w, "Error encoding: "+err.Error(), http.StatusInternalServerError)
	}

}

type JsonObject map[string]interface{}

// post this json to the body
//the frame type with extension ".t" are handled a bit differnt
// it generats the timestamp based bitmap_ids
//[{ "db": "3", "frame":"b.n","profile_id": 122,"filter":0, "bitmap_id":123},
//	{ "db": "3", "frame":"b.n","profile_id": 122,"filter":2, "bitmap_id":124},
//	{ "db": "3", "frame":"t.t","profile_id": 122,"filter":2, "bitmap_id":124, "timestamp":"2014-04-03 13:01:04"}]'
//
func bitmaps(frame string, obj JsonObject) chan uint64 {
	c := make(chan uint64)

	go func() {
		const shortFormT = "2006-01-02T15:04:05"
		const shortFormS = "2006-01-02 15:04:05"
		t := float64(obj["bitmap_id"].(float64))
		base_id := uint64(t)

		if strings.HasSuffix(frame, ".t") {
			timestamp := obj["timestamp"].(string)
			if timestamp == "2014-01-01 00:00:00" { //skip the default timestamp
				c <- base_id
			} else {
				quantum := index.YMDH
				if val, ok := obj["time_granulatrity"]; ok {
					switch val {
					case "Y":
						quantum = index.Y
					case "M":
						quantum = index.YM
					case "D":
						quantum = index.YMD
					}
				}
				shortForm := shortFormS
				if strings.Contains(timestamp, "T") {
					shortForm = shortFormT
				}
				atime, _ := time.Parse(shortForm, timestamp)

				for _, id := range index.GetTimeIds(base_id, atime, quantum) {
					c <- id
				}
			}
		} else {
			c <- base_id
		}

		close(c)
	}()

	return c
}

func (self *WebService) HandleSetBit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var args []JsonObject

	err := decoder.Decode(&args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//[{ "db": "3", "frame":"brand.","profile_id": 122,"filter":0, "bitmap_id":123}]
	var results []interface{}
	for _, obj := range args {
		if obj["profile_id"] == nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		t := float64(obj["profile_id"].(float64))
		profile_id := uint64(t)

		if obj["db"] == nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		db := obj["db"].(string)

		if obj["frame"] == nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return

		}
		frame := obj["frame"].(string)

		if obj["filter"] == nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		t = float64(obj["filter"].(float64))
		filter := int(t)

		for bitmap_id := range bitmaps(frame, obj) {
			pql := fmt.Sprintf("set(%d, %s, %d, %d)", bitmap_id, frame, filter, profile_id)
			start := time.Now()
			result, err := self.service.Executor.RunPQL(db, pql)
			delta := time.Since(start)
			util.SendTimer("executor_setbit", delta.Nanoseconds())

			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			results = append(results, result)
			//	results = append(results, db+" "+pql)
		}

	}

	encoder := json.NewEncoder(w)
	err = encoder.Encode(results)
	if err != nil {
		log.Fatal("Error encoding stats")
	}

}

func (self *WebService) HandleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}
	encoder := json.NewEncoder(w)
	m := &runtime.MemStats{}
	runtime.ReadMemStats(m)

	//self.Report(fmt.Sprintf("%s.goroutines", prefix),
	//	float64(runtime.NumGoroutine()), now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.allocated", prefix),
	//	float64(memStats.Alloc), now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.mallocs", prefix),
	//	float64(memStats.Mallocs), now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.frees", prefix),
	//	float64(memStats.Frees), now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.gc.total_pause", prefix),
	//	float64(memStats.PauseTotalNs)/nsInMs, now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.heap", prefix),
	//	float64(memStats.HeapAlloc), now, context, dimensions)
	//self.Report(fmt.Sprintf("%s.memory.stack", prefix),
	//	float64(memStats.StackInuse), now, context, dimensions)

	//stats := map[string]interface{}{
	//	"num_goroutines": runtime.NumGoroutine(),
	//	"memory_allocated": m.Sys,
	//	"memory_": m.Alloc
	//}

	err := encoder.Encode(m)
	if err != nil {
		log.Fatal("Error encoding stats")
	}
}

func (self *WebService) HandleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}
	spew.Fdump(w, self.service.Cluster)
}

func (self *WebService) HandleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}

	fmt.Fprintf(w, "Pilosa v.("+self.service.version+")\n")
}

func (self *WebService) HandleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}
	spew.Dump("TEST!")

	msg := new(db.Message)
	msg.Data = "mystring"
	self.service.Transport.Push(msg)

	msg2 := new(db.Message)
	msg2.Data = 789
	self.service.Transport.Push(msg2)
}

func (self *WebService) HandleProcesses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}
	encoder := json.NewEncoder(w)
	processes := self.service.ProcessMap.GetMetadata()
	err := encoder.Encode(processes)
	if err != nil {
		http.Error(w, "Error Encoding", http.StatusBadRequest)
	}
}

func (self *WebService) HandlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Only GET allowed", http.StatusMethodNotAllowed)
		return
	}
	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	process_string := r.Form.Get("process")
	process_id, err := util.ParseGUID(process_string)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = self.service.ProcessMap.GetProcess(&process_id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	duration, err := self.service.Ping(&process_id)
	if err != nil {
		spew.Fdump(w, err)
		return
	}
	encoder := json.NewEncoder(w)
	encoder.Encode(map[string]float64{"duration": duration.Seconds()})
}

func (self *WebService) HandleListen(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(`
	<html><head><title>Pilosa - streaming client</title></head><body>
	<style type="text/css">
		dt.inbox { background-color: #efe; }
		dt.outbox { background-color: #eef; }
	</style>
	<script src="//ajax.googleapis.com/ajax/libs/jquery/1.10.2/jquery.min.js"></script>
	<script type="text/javascript">
		$dl = $("<dl/>")
		$dl.on('click', 'dt', function(e) {
			$(this).next().toggle()
		})
		$("body").append($dl)
		ws = new WebSocket("ws://" + window.location.host + "/listen/ws")
		ws.onmessage = function(mes) {
			obj = JSON.parse(mes.data)
			if (obj.host) {
				$dl.append('<dt class="outbox">&rarr;' + obj.type + ' (to: ' + obj.host + ')</dt>')
			} else {
				$dl.append('<dt class="inbox">&larr;' + obj.type + '</dt>')
			}
			$dl.append('<dd style="display:none;"><pre>' + obj.dump + obj.host + '</pre></dd>')
		}
	</script>
	</body></html>
	`))
}

func (self *WebService) streamer(writer func(map[string]interface{}) error) {
	inbox := make(chan interface{}, 10)
	outbox := make(chan interface{}, 10)
	notify.Start("inbox", inbox)
	notify.Start("outbox", outbox)
	var obj interface{}
	var data interface{}
	var inmessage *db.Message
	var outmessage *db.Envelope
	var host string
	for {
		select {
		case obj = <-outbox:
			outmessage = obj.(*db.Envelope)
			data = outmessage.Message.Data
			host = outmessage.Host.String()
		case obj = <-inbox:
			inmessage = obj.(*db.Message)
			data = inmessage.Data
			host = ""
		}

		typ := reflect.TypeOf(data)

		err := writer(map[string]interface{}{
			"type": typ.String(),
			"dump": spew.Sdump(data),
			"host": host,
		})
		if err != nil {
			log.Println("stopping")
			notify.Stop("inbox", inbox)
			notify.Stop("outbox", outbox)
			drain(inbox)
			drain(outbox)
			return
		}
	}
}

func (self *WebService) HandleListenWS(w http.ResponseWriter, r *http.Request) {
	defer func() {
		err := recover()
		spew.Dump(err)
	}()
	ws, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if _, ok := err.(websocket.HandshakeError); ok {
		http.Error(w, "Not a websocket handshake", 400)
		return
	} else if err != nil {
		log.Println(err)
		return
	}
	self.streamer(func(data map[string]interface{}) error {
		return ws.WriteJSON(data)
	})
}

func drain(ch chan interface{}) {
	for {
		switch {
		case <-ch:
		default:
			return
		}
	}
}

func (self *WebService) HandleListenStream(w http.ResponseWriter, r *http.Request) {
	defer func() {
		err := recover()
		spew.Dump(err)
	}()
	writer := json.NewEncoder(w)
	flusher := w.(http.Flusher)
	self.streamer(func(data map[string]interface{}) error {
		err := writer.Encode(data)
		flusher.Flush()
		return err
	})
}

func (self *WebService) HandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(`
	<html><head><title>Pilosa - status</title></head><body>
	<style type="text/css">
	td {
		background: #eee;
	}
	</style>
	<script src="//ajax.googleapis.com/ajax/libs/jquery/1.10.2/jquery.min.js"></script>
	<script type="text/javascript">
		$table = $("<table><tr><th>process id</th><th>host</th><th>tcp port</th><th>http port</th><th>latency</th></tr></table>")
		$("body").append($table)
		$.ajax('/processes', {
			dataType: "json",
			success: function(resp) {
				$.each(resp, function(index, value) {
					$table.append('<tr><td>' + index + '</td><td>' + value.host + '</td><td>' + value.port_tcp + '</td><td>' + value.port_http + '</td><td><button class="pinger"/></td></tr>')
				})
			}
		})
		$table.on('click', 'button.pinger', function(e) {
			var $td = $(this).parent()
			var $tr = $td.parent()
			var id = $tr.children('td').eq(0).text()

			$.ajax('/ping?process=' +  id, {
				dataType: 'json',
				success: function(resp) {
					$td.html(resp.duration)
				}
			})
		})
	</script>
	</body></html>
	`))
}
