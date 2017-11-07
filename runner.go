package hazana

import (
	"flag"
	"fmt"
	"log"
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
	metrics         map[string]*Metrics
	resultsPipeline func(r result) result
}

// Run starts attacking a service using an Attack implementation and a configuration.
// Return a report with statistics per sample and the configuration used.
func Run(a Attack, c Config) RunReport {
	if c.Verbose {
		log.Println("hazana - load runner")
	}
	r := new(runner)
	r.config = c
	r.prototype = a

	// do a test if the flag says so
	if *oSample > 0 {
		r.test(*oSample)
		os.Exit(0)
		// unreachable
		return RunReport{}
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
	return r.run()
}

func (r *runner) init() {
	r.next = make(chan bool)
	r.quit = make(chan bool)
	r.results = make(chan result)
	r.attackers = []Attack{}
	r.metrics = map[string]*Metrics{}
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
	go attack(attacker, r.next, r.quit, r.results, r.config.timeout())
}

// addResult is called from a dedicated goroutine.
func (r *runner) addResult(s result) result {
	m, ok := r.metrics[s.doResult.RequestLabel]
	if !ok {
		m = new(Metrics)
		r.metrics[s.doResult.RequestLabel] = m
	}
	m.add(s)
	return s
}

// test uses the Attack to perform {count} calls and report its result
// it is intended for development of an Attack implementation.
func (r *runner) test(count int) {
	probe := r.prototype.Clone()
	if err := probe.Setup(r.config); err != nil {
		log.Printf("Test attack setup failed [%v]", err)
		return
	}
	defer probe.Teardown()
	for s := count; s > 0; s-- {
		now := time.Now()
		result := probe.Do()
		log.Printf("Test attack call [%s] took [%v] with status [%v] and error [%v]\n", result.RequestLabel, time.Now().Sub(now), result.StatusCode, result.Error)
	}
}

// run offers the complete flow of a load test.
func (r *runner) run() RunReport {
	go r.collectResults()
	r.rampUp()
	r.fullAttack()
	r.quitAttackers()
	r.tearDownAttackers()
	return r.reportMetrics()
}

func (r *runner) fullAttack() {
	if r.config.Verbose {
		log.Printf("begin full attack of [%d] remaining seconds\n", r.config.AttackTimeSec-r.config.RampupTimeSec)
	}
	fullAttackStartedAt = time.Now()
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
	strategy := r.config.rampupStrategy()
	if r.config.Verbose {
		log.Printf("begin rampup of [%d] seconds using strategy [%s]\n", r.config.RampupTimeSec, strategy)
	}
	switch strategy {
	case "linear":
		linearIncreasingGoroutinesAndRequestsPerSecondStrategy{}.execute(r)
	case "exp2":
		spawnAsWeNeedStrategy{}.execute(r)
	}
	// restore pipeline function incase it was changed by the rampup strategy
	r.resultsPipeline = r.addResult
	if r.config.Verbose {
		log.Printf("end rampup ending up with [%d] attackers\n", len(r.attackers))
	}
}

func (r *runner) quitAttackers() {
	if r.config.Verbose {
		log.Printf("stopping attackers [%d]\n", len(r.attackers))
	}
	for range r.attackers {
		r.quit <- true
	}
}

func (r *runner) tearDownAttackers() {
	if r.config.Verbose {
		log.Printf("tearing down attackers [%d]\n", len(r.attackers))
	}
	for i, each := range r.attackers {
		if err := each.Teardown(); err != nil {
			log.Printf("ERROR failed to teardown attacker [%d]:%v\n", i, err)
		}
	}
}

func (r *runner) reportMetrics() RunReport {
	for _, each := range r.metrics {
		each.updateLatencies()
	}
	return RunReport{
		StartedAt:     fullAttackStartedAt,
		FinishedAt:    time.Now(),
		Configuration: r.config,
		Metrics:       r.metrics,
	}
}

func (r *runner) collectResults() {
	for {
		r.resultsPipeline(<-r.results)
	}
}
