// Package geoip resolves a public IP address to an approximate location
// using the offline ip2region database (conf/ip2region.xdb, loaded at
// startup from https://github.com/lionsoul2014/ip2region, Apache-2.0/MIT
// dual licensed) for country/province/city/ISP text, plus an embedded
// world-cities table (lib/geoip/data/worldcities.csv, MIT licensed, see
// data/worldcities.LICENSE.txt) and a hand-authored Chinese city table for
// city-level coordinates. No network access is made at lookup time.
package geoip

import (
	"bufio"
	"embed"
	"encoding/csv"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

//go:embed data/worldcities.csv
var worldCitiesFS embed.FS

var (
	searcher *xdb.Searcher
	mu       sync.Mutex
)

type cityEntry struct {
	lat, lng float64
	pop      float64
}

var (
	worldCityByNameCountry map[string]cityEntry // key: lower(city_ascii)+"|"+lower(country)
	worldCityByName        map[string]cityEntry // key: lower(city_ascii), highest population wins
	cityIndexOnce          sync.Once
)

// Result is the outcome of a Lookup.
type Result struct {
	Country string
	// CountryCode is the ISO 3166-1 alpha-2 code (e.g. "CN", "US"), empty
	// when unresolved.
	CountryCode string
	Province    string
	City        string
	ISP         string
	Lat         float64
	Lng         float64
	// HasGeo is true when Lat/Lng were resolved (country private/unknown IPs
	// leave it false and Lat/Lng at zero).
	HasGeo bool
}

// Init loads the xdb database from the given file path. If it fails, Lookup
// silently returns a zero-value, not-found Result for every address instead
// of erroring out, so a missing or corrupt database never blocks the server
// from starting.
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

func loadWorldCities() {
	worldCityByNameCountry = make(map[string]cityEntry)
	worldCityByName = make(map[string]cityEntry)
	f, err := worldCitiesFS.Open("data/worldcities.csv")
	if err != nil {
		return
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReader(f))
	header, err := r.Read()
	if err != nil {
		return
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[h] = i
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) <= col["lng"] {
			continue
		}
		lat, err1 := strconv.ParseFloat(rec[col["lat"]], 64)
		lng, err2 := strconv.ParseFloat(rec[col["lng"]], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		pop, _ := strconv.ParseFloat(rec[col["pop"]], 64)
		city := strings.ToLower(rec[col["city_ascii"]])
		country := strings.ToLower(rec[col["country"]])
		entry := cityEntry{lat: lat, lng: lng, pop: pop}
		if city == "" {
			continue
		}
		worldCityByNameCountry[city+"|"+country] = entry
		if existing, ok := worldCityByName[city]; !ok || entry.pop > existing.pop {
			worldCityByName[city] = entry
		}
	}
}

// accentFold maps common accented Latin letters to their unaccented base, so
// e.g. ip2region's "São Paulo" matches the world-cities table's ASCII
// "Sao Paulo".
var accentFold = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a",
	"è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u",
	"ý", "y", "ÿ", "y",
	"ñ", "n", "ç", "c", "ß", "ss", "ğ", "g", "ş", "s", "ı", "i",
)

// cityLatLng resolves city-level coordinates. isChina picks the
// hand-authored Chinese city table (ip2region returns Chinese city names for
// domestic IPs); otherwise the embedded world-cities table is used, matched
// by city name and, when available, disambiguated by country.
func cityLatLng(isChina bool, city, country string) (lat, lng float64, ok bool) {
	if city == "" {
		return 0, 0, false
	}
	if isChina {
		if coord, exist := chinaCityCentroids[city]; exist {
			return coord[0], coord[1], true
		}
		// ip2region sometimes omits the trailing administrative suffix
		for _, suffix := range []string{"市", "自治州", "地区", "盟"} {
			if trimmed := strings.TrimSuffix(city, suffix); trimmed != city {
				if coord, exist := chinaCityCentroids[trimmed+"市"]; exist {
					return coord[0], coord[1], true
				}
			}
		}
		return 0, 0, false
	}
	cityIndexOnce.Do(loadWorldCities)
	key := accentFold.Replace(strings.ToLower(city))
	countryKey := accentFold.Replace(strings.ToLower(country))
	if entry, exist := worldCityByNameCountry[key+"|"+countryKey]; exist {
		return entry.lat, entry.lng, true
	}
	if entry, exist := worldCityByName[key]; exist {
		return entry.lat, entry.lng, true
	}
	return 0, 0, false
}

// Lookup resolves addr (either "ip" or "ip:port") to an approximate
// location. Private, loopback, unspecified, or unresolvable addresses (or a
// database that hasn't loaded) return a zero-value Result with HasGeo=false.
func Lookup(addr string) Result {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return Result{}
	}

	mu.Lock()
	s := searcher
	mu.Unlock()
	if s == nil {
		return Result{}
	}

	region, err := s.Search(host)
	if err != nil {
		return Result{}
	}
	// format: Country|Province|City|ISP|ISO-alpha2-code
	fields := strings.Split(region, "|")
	if len(fields) < 5 || fields[0] == "" || fields[0] == "0" {
		return Result{}
	}
	res := Result{Country: fields[0]}
	if fields[1] != "0" {
		res.Province = fields[1]
	}
	if fields[2] != "0" {
		res.City = fields[2]
	}
	if fields[3] != "0" {
		res.ISP = fields[3]
	}
	code := strings.ToUpper(strings.TrimSpace(fields[4]))
	res.CountryCode = code
	isChina := code == "CN"
	// cityLatLng matches against the world-cities table's English country
	// names, so resolve the city before swapping Country to its Chinese
	// display name below.
	cityCountry := res.Country

	if !isChina {
		if zh, exist := countryNamesZH[code]; exist {
			res.Country = zh
		}
	}

	if lat, lng, ok := cityLatLng(isChina, res.City, cityCountry); ok {
		res.Lat, res.Lng, res.HasGeo = lat, lng, true
		return res
	}
	if coord, exist := countryCentroids[code]; exist {
		res.Lat, res.Lng, res.HasGeo = coord[0], coord[1], true
	}
	return res
}
