package hystrix

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// CircuitBreaker is created for each ExecutorPool to track whether requests
// should be attempted, or rejected if the Health of the circuit is too low.
type CircuitBreaker struct {
	Name                   string
	open                   bool
	forceOpen              bool
	mutex                  *sync.RWMutex
	openedOrLastTestedTime int64

	executorPool *executorPool
	metrics      *metrics
}

var (
	circuitBreakersMutex *sync.RWMutex
	circuitBreakers      map[string]*CircuitBreaker
)

func init() {
	circuitBreakersMutex = &sync.RWMutex{}
	circuitBreakers = make(map[string]*CircuitBreaker)
}

// GetCircuit returns the circuit for the given command and whether this call created it.
func GetCircuit(name string) (*CircuitBreaker, bool, error) {
	circuitBreakersMutex.RLock()
	_, ok := circuitBreakers[name]
	if !ok {
		circuitBreakersMutex.RUnlock()
		circuitBreakersMutex.Lock()
		defer circuitBreakersMutex.Unlock()
		circuitBreakers[name] = newCircuitBreaker(name)
	} else {
		defer circuitBreakersMutex.RUnlock()
	}

	return circuitBreakers[name], !ok, nil
}

func Flush() {
	circuitBreakersMutex.Lock()
	defer circuitBreakersMutex.Unlock()

	for name, cb := range circuitBreakers {
		cb.metrics.Reset()
		cb.executorPool.Metrics.Reset()
		delete(circuitBreakers, name)
	}
}

// newCircuitBreaker creates a CircuitBreaker with associated Health
func newCircuitBreaker(name string) *CircuitBreaker {
	c := &CircuitBreaker{}
	c.Name = name
	c.metrics = newMetrics(name)
	c.executorPool = newExecutorPool(name)
	c.mutex = &sync.RWMutex{}

	return c
}

// toggleForceOpen allows manually causing the fallback logic for all instances
// of a given command.
func (circuit *CircuitBreaker) toggleForceOpen(toggle bool) error {
	circuit, _, err := GetCircuit(circuit.Name)
	if err != nil {
		return err
	}

	circuit.forceOpen = toggle
	return nil
}

// isOpen is called before any Command execution to check whether or
// not it should be attempted. An "open" circuit means it is disabled.
func (circuit *CircuitBreaker) isOpen() bool {
	circuit.mutex.RLock()
	o := circuit.forceOpen || circuit.open
	circuit.mutex.RUnlock()

	if o {
		return true
	}

	if circuit.metrics.Requests().Sum(time.Now()) < getSettings(circuit.Name).RequestVolumeThreshold {
		return false
	}

	if !circuit.metrics.IsHealthy(time.Now()) {
		// too many failures, open the circuit
		circuit.setOpen()
		return true
	}

	return false
}

func (circuit *CircuitBreaker) AllowRequest() bool {
	return !circuit.isOpen() || circuit.allowSingleTest()
}

func (circuit *CircuitBreaker) allowSingleTest() bool {
	circuit.mutex.RLock()
	defer circuit.mutex.RUnlock()

	now := time.Now().UnixNano()
	if circuit.open && now > circuit.openedOrLastTestedTime+getSettings(circuit.Name).SleepWindow.Nanoseconds() {
		swapped := atomic.CompareAndSwapInt64(&circuit.openedOrLastTestedTime, circuit.openedOrLastTestedTime, now)
		if swapped {
			log.Printf("hystrix-go: allowing single test to possibly close circuit %v", circuit.Name)
		}
		return swapped
	}

	return false
}

func (circuit *CircuitBreaker) setOpen() {
	circuit.mutex.Lock()
	defer circuit.mutex.Unlock()

	log.Printf("hystrix-go: opening circuit %v", circuit.Name)

	circuit.openedOrLastTestedTime = time.Now().UnixNano()
	circuit.open = true
}

func (circuit *CircuitBreaker) setClose() {
	circuit.mutex.Lock()
	defer circuit.mutex.Unlock()

	log.Printf("hystrix-go: closing circuit %v", circuit.Name)

	circuit.open = false
	circuit.metrics.Reset()
}

func (circuit *CircuitBreaker) ReportEvent(eventType string, start time.Time, runDuration time.Duration) error {
	if eventType == "success" && circuit.isOpen() {
		circuit.setClose()
	}

	circuit.metrics.Updates <- &commandExecution{
		Type:        eventType,
		Start:       start,
		RunDuration: runDuration,
	}

	return nil
}
