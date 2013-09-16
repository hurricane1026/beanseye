package memcache

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scheduler: route request to nodes
type Scheduler interface {
	Feedback(host *Host, key string, adjust float64) // feedback for auto routing
	GetHostsByKey(key string) []*Host                // route a key to hosts
	DivideKeysByBucket(keys []string) [][]string     // route some keys to group of hosts
	Stats() map[string][]float64                     // internal status
}

type emptyScheduler struct{}

func (c emptyScheduler) Feedback(host *Host, key string, adjust float64) {}

func (c emptyScheduler) Stats() map[string][]float64 { return nil }

// route request by Mod of HASH
type ModScheduler struct {
	hosts      []*Host
	hashMethod HashMethod
	emptyScheduler
}

func NewModScheduler(hosts []string, hashname string) Scheduler {
	var c ModScheduler
	c.hosts = make([]*Host, len(hosts))
	c.hashMethod = hashMethods[hashname]
	for i, h := range hosts {
		c.hosts[i] = NewHost(h)
	}
	return &c
}

func (c *ModScheduler) GetHostsByKey(key string) []*Host {
	h := c.hashMethod([]byte(key))
	r := make([]*Host, 1)
	r[0] = c.hosts[h%uint32(len(c.hosts))]
	return r
}

func (c *ModScheduler) DivideKeysByBucket(keys []string) [][]string {
	n := len(c.hosts)
	rs := make([][]string, n)
	for _, key := range keys {
		h := c.hashMethod([]byte(key)) % uint32(n)
		rs[h] = append(rs[h], key)
	}
	return rs
}

// internal status
func (c *ModScheduler) Stats() map[string][]float64 {
	r := make(map[string][]float64)
	for i, h := range c.hosts {
		r[h.Addr] = make([]float64, len(c.hosts))
		r[h.Addr][i] = 1
	}
	return r
}

type uint64Slice []uint64

func (l uint64Slice) Len() int {
	return len(l)
}

func (l uint64Slice) Less(i, j int) bool {
	return l[i] < l[j]
}

func (l uint64Slice) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

// route requests by consistant hash
type ConsistantHashScheduler struct {
	hosts      []*Host
	index      []uint64
	hashMethod HashMethod
	emptyScheduler
}

const VIRTUAL_NODES = 100

func NewConsistantHashScheduler(hosts []string, hashname string) Scheduler {
	var c ConsistantHashScheduler
	c.hosts = make([]*Host, len(hosts))
	c.index = make([]uint64, len(hosts)*VIRTUAL_NODES)
	c.hashMethod = hashMethods[hashname]
	for i, h := range hosts {
		c.hosts[i] = NewHost(h)
		for j := 0; j < VIRTUAL_NODES; j++ {
			v := c.hashMethod([]byte(fmt.Sprintf("%s-%d", h, j)))
			ps := strings.SplitN(h, ":", 2)
			host := ps[0]
			port := ps[1]
			if port == "11211" {
				v = c.hashMethod([]byte(fmt.Sprintf("%s-%d", host, j)))
			}
			c.index[i*VIRTUAL_NODES+j] = (uint64(v) << 32) + uint64(i)
		}
	}
	sort.Sort(uint64Slice(c.index))
	if !sort.IsSorted(uint64Slice(c.index)) {
		panic("sort failed")
	}
	return &c
}

func (c *ConsistantHashScheduler) getHostIndex(key string) int {
	h := uint64(c.hashMethod([]byte(key))) << 32
	N := len(c.index)
	i := sort.Search(N, func(k int) bool { return c.index[k] >= h })
	if i == N {
		i = 0
	}
	return int(c.index[i] & 0xffffffff)
}

func (c *ConsistantHashScheduler) GetHostsByKey(key string) []*Host {
	r := make([]*Host, 1)
	i := c.getHostIndex(key)
	r[0] = c.hosts[i]
	return r
}

func (c *ConsistantHashScheduler) DivideKeysByBucket(keys []string) [][]string {
	n := len(c.hosts)
	rs := make([][]string, n)
	for _, key := range keys {
		i := c.getHostIndex(key)
		rs[i] = append(rs[i], key)
	}
	return rs
}

// route request by configure by hand
type ManualScheduler struct {
	hosts      map[string]*Host
	buckets    [][]string
	hashMethod HashMethod
}

func NewManualScheduler(config map[string][]int) *ManualScheduler {
	c := new(ManualScheduler)
	count := 1
	c.hosts = make(map[string]*Host)
	for addr, bs := range config {
		c.hosts[addr] = NewHost(addr)
		for _, b := range bs {
			if b >= count {
				count = b + 1
			}
		}
	}
	// dispatch
	c.buckets = make([][]string, count)
	for addr, bs := range config {
		for _, bi := range bs {
			c.buckets[bi] = append(c.buckets[bi], addr)
		}
	}
	c.hashMethod = fnv1a1
	return c
}

func (c *ManualScheduler) GetHostsByKey(key string) (host []*Host) {
	i := getBucketByKey(c.hashMethod, len(c.buckets), key)

	N := len(c.buckets[i])
	hs := make([]*Host, N)
	rnd := int(c.hashMethod([]byte(key)) % uint32(N))
	for j, addr := range c.buckets[i] {
		hs[(j+rnd)%N] = c.hosts[addr]
	}
	return hs
}

func (c *ManualScheduler) DivideKeysByBucket(keys []string) [][]string {
	return divideKeysByBucket(c.hashMethod, len(c.buckets), keys)
}

func (c *ManualScheduler) Feedback(host *Host, key string, adjust float64) {

}

func (c *ManualScheduler) Stats() map[string][]float64 {
	r := make(map[string][]float64, len(c.hosts))
	for addr, _ := range c.hosts {
		r[addr] = make([]float64, len(c.buckets))
	}
	for i, hs := range c.buckets {
		for _, h := range hs {
			r[h][i] = 1
		}
	}
	return r
}

type Feedback struct {
	hostIndex   int
	bucketIndex int
	adjust      float64
}

// route requests by auto discoved infomation, used in beansdb
type AutoScheduler struct {
	n          int
	hosts      []*Host
	buckets    [][]int
	stats      [][]float64
	last_check time.Time
	hashMethod HashMethod
	feedChan   chan *Feedback
}

func NewAutoScheduler(config []string, bs int) *AutoScheduler {
	c := new(AutoScheduler)
	c.n = len(config)
	c.hosts = make([]*Host, c.n)
	c.buckets = make([][]int, bs)
	c.stats = make([][]float64, bs)
	for i := 0; i < bs; i++ {
		c.buckets[i] = make([]int, c.n)
		c.stats[i] = make([]float64, c.n)
	}
	for i, addr := range config {
		c.hosts[i] = NewHost(addr)
		for j := 0; j < bs; j++ {
			c.buckets[j][i] = i
			c.stats[j][i] = 0
		}
	}
	c.hashMethod = fnv1a1
	go c.procFeedback()

	c.check()
	go func() {
		for {
			c.check()
			time.Sleep(10 * 1e9)
		}
	}()
	return c
}

func getBucketByKey(hash_func HashMethod, bs int, key string) int {
	bucketWidth := 0
	for bs > 1 {
		bucketWidth++
		bs /= 2
	}
	if len(key) > bucketWidth/4 && key[0] == '@' {
		return hextoi(key[1 : bucketWidth/4+1])
	}
	if len(key) >= 1 && key[0] == '?' {
		key = key[1:]
	}
	h := hash_func([]byte(key))
	return (int)(h >> (uint)(32-bucketWidth))
}

func (c *AutoScheduler) GetHostsByKey(key string) []*Host {
	i := getBucketByKey(c.hashMethod, len(c.buckets), key)
	cnt := len(c.hosts)
	hosts := make([]*Host, cnt)
	for j := 0; j < cnt; j++ {
		hosts[j] = c.hosts[c.buckets[i][j]]
	}
	return hosts
}

func divideKeysByBucket(hash_func HashMethod, bs int, keys []string) [][]string {
	rs := make([][]string, bs)
	for _, key := range keys {
		b := getBucketByKey(hash_func, bs, key)
		rs[b] = append(rs[b], key)
	}
	return rs
}

func (c *AutoScheduler) DivideKeysByBucket(keys []string) [][]string {
	return divideKeysByBucket(c.hashMethod, len(c.buckets), keys)
}

func (c *AutoScheduler) Stats() map[string][]float64 {
	r := make(map[string][]float64)
	for _, h := range c.hosts {
		r[h.Addr] = make([]float64, len(c.buckets))
	}
	for i, st := range c.stats {
		for j, w := range st {
			r[c.hosts[j].Addr][i] = w
		}
	}
	return r
}

func swap(a []int, j, k int) {
	a[j], a[k] = a[k], a[j]
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func (c *AutoScheduler) hostIndex(host *Host) int {
	for i, h := range c.hosts {
		if h == host {
			return i
		}
	}
	return -1
}

func (c *AutoScheduler) procFeedback() {
	c.feedChan = make(chan *Feedback, 1024)
	for {
		fb := <-c.feedChan
		c.feedback(fb.hostIndex, fb.bucketIndex, fb.adjust)
	}
}

func (c *AutoScheduler) Feedback(host *Host, key string, adjust float64) {
	index := getBucketByKey(c.hashMethod, len(c.buckets), key)
	i := c.hostIndex(host)
	if i < 0 {
		return
	}
	//c.feedback(i, index, adjust)
	c.feedChan <- &Feedback{hostIndex: i, bucketIndex: index, adjust: adjust}
}

func (c *AutoScheduler) feedback(i, index int, adjust float64) {
	stats := c.stats[index]
	old := stats[i]
	if adjust >= 0 {
		//log.Print("reset ", index, " ", c.hosts[i].Addr, " ", stats[i], adjust)
		stats[i] = (stats[i] + adjust) / 2
	} else {
		stats[i] += adjust
	}
	buckets := c.buckets[index]
	k := 0
	for k = 0; k < len(c.hosts); k++ {
		if buckets[k] == i {
			break
		}
	}
	if stats[i]-old > 0 {
		for k > 0 && stats[buckets[k]] > stats[buckets[k-1]] {
			swap(buckets, k, k-1)
			k--
		}
	} else {
		for k < len(c.hosts)-1 && stats[buckets[k]] < stats[buckets[k+1]] {
			swap(buckets, k, k+1)
			k++
		}
	}
}

func hextoi(hex string) int {
	r := rune(0)
	for _, c := range hex {
		r *= 16
		switch {
		case c >= '0' && c <= '9':
			r += c - '0'
		case c >= 'A' && c <= 'F':
			r += 10 + c - 'A'
		case c >= 'a' && c <= 'f':
			r += 10 + c - 'a'
		}
	}
	return int(r)
}

func (c *AutoScheduler) listHost(host *Host, dir string) {
	rs, err := host.Get(dir)
	if err != nil || rs == nil {
		return
	}
	for _, line := range bytes.SplitN(rs.Body, []byte("\n"), 17) {
		if bytes.Count(line, []byte(" ")) < 2 || line[1] != '/' {
			continue
		}
		vv := bytes.SplitN(line, []byte(" "), 3)
		cnt, _ := strconv.ParseFloat(string(vv[2]), 64)
		adjust := float64(math.Sqrt(cnt))
		c.Feedback(host, dir+string(vv[0]), adjust)
	}
}

func (c *AutoScheduler) check() {
	defer func() {
		if e := recover(); e != nil {
			log.Print("error while check()", e)
		}
	}()
	bs := len(c.buckets)
	bucketWidth := 0
	for bs > 1 {
		bucketWidth++
		bs /= 2
	}
	count := 1 << (uint)(bucketWidth-4)
	w := bucketWidth/4 - 1
	format := fmt.Sprintf("@%%0%dx", w)
	for _, host := range c.hosts {
		if w < 1 {
			c.listHost(host, "@")
		} else {
			for i := 0; i < count; i++ {
				key := fmt.Sprintf(format, i)
				c.listHost(host, key)
			}
		}
	}
	log.Println("---checking---")
	for i, host_ids := range c.buckets {
		log.Print("bucket ", i, " : ")
		for _, id := range host_ids {
			log.Print(" ", c.hosts[id].Addr, " ")
		}
	}
	log.Println("buckets", c.buckets)
	c.last_check = time.Now()
}
