// Package geoip resolves a public IP address to an approximate country-level
// location using the offline ip2region database (data/ip2region_v4.xdb from
// https://github.com/lionsoul2014/ip2region, Apache-2.0/MIT dual licensed).
// No network access is made at lookup time.
package geoip

import (
	"net"
	"strings"
	"sync"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

var (
	searcher *xdb.Searcher
	mu       sync.Mutex
)

// Init loads the xdb database from the given file path. If it fails, Lookup
// silently returns ok=false for every address instead of erroring out, so a
// missing or corrupt database never blocks the server from starting.
func Init(xdbPath string) error {
	s, err := xdb.NewWithFileOnly(xdb.IPv4, xdbPath)
	if err != nil {
		return err
	}
	mu.Lock()
	searcher = s
	mu.Unlock()
	return nil
}

// Lookup resolves addr (either "ip" or "ip:port") to its country name and an
// approximate country-centroid latitude/longitude. ok is false for private,
// loopback, unspecified, or unresolvable addresses, or when the database
// hasn't been loaded.
func Lookup(addr string) (country string, lat, lng float64, ok bool) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return "", 0, 0, false
	}

	mu.Lock()
	s := searcher
	mu.Unlock()
	if s == nil {
		return "", 0, 0, false
	}

	region, err := s.Search(host)
	if err != nil {
		return "", 0, 0, false
	}
	// format: Country|Province|City|ISP|ISO-alpha2-code
	fields := strings.Split(region, "|")
	if len(fields) < 5 || fields[0] == "" || fields[0] == "0" {
		return "", 0, 0, false
	}
	country = fields[0]
	code := strings.ToUpper(strings.TrimSpace(fields[4]))
	if coord, exist := countryCentroids[code]; exist {
		return country, coord[0], coord[1], true
	}
	return country, 0, 0, false
}
