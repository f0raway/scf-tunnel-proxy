package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const computeRate = 0.125 * 0.00011108
const trafficRate = 0.80 / (1024 * 1024 * 1024)
const requestRate = 0.0133 / 10000
const budgetWindow = 30*computeRate + 2*256*1024*trafficRate

type usage struct {
	Month     string  `json:"month"`
	Estimated float64 `json:"estimated_yuan"`
	Calls     uint64  `json:"invocations"`
	Bytes     uint64  `json:"wire_bytes"`
}
type budgetLease struct {
	m    *meter
	held float64
	done bool
}
type meter struct {
	mu     sync.Mutex
	path   string
	limit  float64
	state  usage
	active map[*budgetLease]bool
}

func openMeter(path string, limit float64) (*meter, error) {
	if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return nil, fmt.Errorf("invalid budget")
	}
	m := &meter{path: path, limit: limit, active: make(map[*budgetLease]bool)}
	b, e := os.ReadFile(path)
	if e == nil {
		if e = json.Unmarshal(b, &m.state); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	m.roll()
	return m, m.save()
}
func (m *meter) roll() {
	month := time.Now().In(time.FixedZone("CST", 28800)).Format("2006-01")
	if m.state.Month != month {
		m.state = usage{Month: month}
		for charge := range m.active {
			m.state.Estimated += charge.held
			m.state.Calls++
		}
	}
}
func (m *meter) save() error {
	b, e := json.MarshalIndent(m.state, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(m.path), 0700); e != nil {
		return e
	}
	if e = os.WriteFile(m.path+".tmp", b, 0600); e != nil {
		return e
	}
	return os.Rename(m.path+".tmp", m.path)
}
func (m *meter) snapshot() usage { m.mu.Lock(); defer m.mu.Unlock(); m.roll(); return m.state }
func estimatedCost(d time.Duration, n int64) float64 {
	return (d.Seconds()+3)*computeRate + float64(n)*2*trafficRate + requestRate
}
func (m *meter) begin() (*budgetLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	amount := budgetWindow + 3*computeRate + requestRate
	if m.state.Estimated+amount > m.limit {
		return nil, fmt.Errorf("本月本地估算额度不足，新的转发暂停")
	}
	prev := m.state
	charge := &budgetLease{m: m, held: amount}
	m.active[charge] = true
	m.state.Estimated += amount
	m.state.Calls++
	if e := m.save(); e != nil {
		m.state = prev
		delete(m.active, charge)
		return nil, e
	}
	return charge, nil
}

// progress replenishes a small rolling reserve before the current one is spent.
// It persists only at a reserve boundary, rather than on every network packet.
func (c *budgetLease) progress(d time.Duration, n int64) error {
	m := c.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.done {
		return net.ErrClosed
	}
	m.roll()
	spent := estimatedCost(d, n)
	if c.held-spent >= budgetWindow/2 {
		return nil
	}
	additional := spent + budgetWindow - c.held
	if m.state.Estimated+additional > m.limit {
		return fmt.Errorf("本月费用保护达到上限，当前隧道停止转发")
	}
	m.state.Estimated += additional
	c.held += additional
	if e := m.save(); e != nil {
		m.state.Estimated -= additional
		c.held -= additional
		return e
	}
	return nil
}
func (c *budgetLease) finish(d time.Duration, n int64) error {
	m := c.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.done {
		return nil
	}
	m.roll()
	c.done = true
	delete(m.active, c)
	m.state.Estimated += estimatedCost(d, n) - c.held
	if m.state.Estimated < 0 {
		m.state.Estimated = 0
	}
	if n > 0 {
		m.state.Bytes += uint64(n)
	}
	return m.save()
}
func (m *meter) reserve() (func(time.Duration, int64) error, error) {
	c, e := m.begin()
	if e != nil {
		return nil, e
	}
	return c.finish, nil
}
