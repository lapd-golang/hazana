package hazana

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"time"

	"go.uber.org/ratelimit"
)

type runner struct {
	config          Config
	attackers       []Attack
	next, quit      chan bool
	results         chan result
	prototype       Attack
	metrics         map[int]*Metrics
	resultsPipeline func(r result) result
}

// Run starts attacking a service using an Attack implementation and a configuration.
func Run(a Attack, c Config) {
	r := new(runner)
	r.config = c
	r.prototype = a

	// do a test if the flag says so
	if *oSample {
		r.test()
		return
	}
	if msg := c.Validate(); len(msg) > 0 {
		for _, each := range msg {
			fmt.Println("[config error]", each)
		}
		fmt.Println()
		flag.Usage()
		os.Exit(0)
	}
	r.init()
	r.run()
}

func (r *runner) init() {
	r.next = make(chan bool)
	r.quit = make(chan bool)
	r.results = make(chan result)
	r.attackers = []Attack{}
	r.metrics = map[int]*Metrics{}
	r.resultsPipeline = r.addResult
}

func (r *runner) spawnAttacker() {
	if r.config.Verbose {
		log.Printf("setup and spawn new attacker [%d]\n", len(r.attackers)+1)
	}
	attacker := r.prototype.Clone()
	if err := attacker.Setup(r.config); err != nil {
		log.Printf("attacker [%d] setup failed with [%v]\n", len(r.attackers)+1, err)
		return
	}
	r.attackers = append(r.attackers, attacker)
	go attack(attacker, r.next, r.quit, r.results)
}

// addResult is called from a dedicated goroutine.
func (r *runner) addResult(s result) result {
	m, ok := r.metrics[s.doResult.RequestIndex]
	if !ok {
		m = new(Metrics)
		r.metrics[s.doResult.RequestIndex] = m
	}
	m.add(s)
	return s
}

// test uses the Attack to perform one call and report its result
// it is intended for development of an Attach implementation.
func (r *runner) test() {
	probe := r.prototype.Clone()
	if err := probe.Setup(r.config); err != nil {
		log.Printf("Test attack setup failed [%v]", err)
		return
	}
	defer probe.Teardown()
	now := time.Now()
	result := probe.Do()
	log.Printf("Test attack call in [%v] with status [%v] and error [%v]\n", time.Now().Sub(now), result.StatusCode, result.Error)
}

func (r *runner) run() {
	go r.collectResults()
	r.rampUp()
	r.fullAttack()
	r.quitAttackers()
	r.tearDownAttackers()
	r.reportMetrics()
}

func (r *runner) fullAttack() {
	if r.config.Verbose {
		log.Printf("begin full attack of [%d] remaining seconds\n", r.config.AttackTimeSec-r.config.RampupTimeSec)
	}
	limiter := ratelimit.New(r.config.RPS) // per second
	doneDeadline := time.Now().Add(time.Duration(r.config.AttackTimeSec-r.config.RampupTimeSec) * time.Second)
	for time.Now().Before(doneDeadline) {
		limiter.Take()
		r.next <- true
	}
	if r.config.Verbose {
		log.Printf("end full attack")
	}
}

func (r *runner) rampUp() {
	if r.config.Verbose {
		log.Printf("begin rampup of [%d] seconds\n", r.config.RampupTimeSec)
	}
	r.spawnAttacker()

	spawnThresholdRatio := 0.9 // 90%
	var rampMetrics *Metrics
	for i := 1; i <= r.config.RampupTimeSec; i++ {
		// collect metrics for each second
		rampMetrics = new(Metrics)
		// change pipeline function to collect local metrics
		r.resultsPipeline = func(rs result) result {
			rampMetrics.add(rs)
			return rs
		}
		// for each second start a new reduced rate limiter
		rps := i * r.config.RPS / r.config.RampupTimeSec
		if rps == 0 { // minimal 1
			rps = 1
		}
		limiter := ratelimit.New(rps) // per second
		oneSecond := time.Now().Add(time.Duration(1 * time.Second))
		for time.Now().Before(oneSecond) {
			limiter.Take()
			r.next <- true
		}
		limiter.Take() // to compensate for the first Take of the new limiter
		rampMetrics.updateLatencies()
		if rampMetrics.Rate > 0 && rampMetrics.Rate < float64(rps) {
			if r.config.Verbose {
				log.Printf("rate [%v] is below target [%v]\n", rampMetrics.Rate, rps)
			}
			// how many attackers can we add to meet the current rps
			if rampMetrics.Rate/float64(rps) < spawnThresholdRatio {
				wanted := int(math.Trunc(float64(rps)/rampMetrics.Rate)) * len(r.attackers)
				for s := len(r.attackers); s < wanted; s++ {
					if s < r.config.MaxAttackers {
						r.spawnAttacker()
					} else {
						if r.config.Verbose {
							log.Printf("reached maximum attackers of [%d]\n", r.config.MaxAttackers)
							break
						}
					}
				}
			}
		}
	}
	// restore pipeline function
	r.resultsPipeline = r.addResult
	if r.config.Verbose {
		log.Printf("end rampup with average rate [%v] after [%v] requests using [%d] attackers\n", rampMetrics.Rate, rampMetrics.Requests, len(r.attackers))
	}
}

func (r *runner) quitAttackers() {
	if r.config.Verbose {
		log.Printf("stopping attackers [%d]\n", len(r.attackers))
	}
	for _ = range r.attackers {
		r.quit <- true
	}
}

func (r *runner) tearDownAttackers() {
	if r.config.Verbose {
		log.Printf("tearing down attackers [%d]\n", len(r.attackers))
	}
	for _, each := range r.attackers {
		if err := each.Teardown(); err != nil {
			log.Printf("ERROR failed to teardown attacker [%v]\n", err)
		}
	}
}

func (r *runner) reportMetrics() {
	var out io.Writer
	if len(r.config.OutputFilename) > 0 {
		file, err := os.Create(r.config.OutputFilename)
		if err != nil {
			log.Fatal("unable to create output file", err)
		}
		defer file.Close()
		out = file
	} else {
		out = os.Stdout
	}
	for _, each := range r.metrics {
		each.updateLatencies()
	}
	data, _ := json.MarshalIndent(r.metrics, "", "\t")
	out.Write(data)
}

func (r *runner) collectResults() {
	for {
		r.resultsPipeline(<-r.results)
	}
}
