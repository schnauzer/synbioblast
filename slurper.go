package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/knakk/sparql"
	"github.com/mediocregopher/radix.v2/redis"
	"github.com/spacemonkeygo/flagfile"
)

// paginated with a scollable cursor as per:
// http://blog.mynarz.net/2016/06/on-generating-sparql.html
const query = `
# tag: fetch
PREFIX dcterms: <http://purl.org/dc/terms/>
PREFIX sbol: <http://sbols.org/v2#>

SELECT
	?uri
	?elements
	?created
WHERE {
	{
		SELECT
			?uri
			?elements
			?created
		WHERE {
			?uri a sbol:ComponentDefinition .
			?uri sbol:sequence ?sequenceUri .
			?sequenceUri sbol:elements ?elements .
			?uri dcterms:created ?created .
		} ORDER BY ASC(str(?created))
	}
}
LIMIT {{.Limit}} OFFSET {{.Offset}}
`

type queryParams struct {
	Limit, Offset int
}

// TODO: deduplicate these
var (
	blastdbDir = flag.String("blastdb.path", "/var/synbioblast/blastdbs",
		"directory where blast dbs are stored")
	blastdbName = flag.String("blastdb.name", "SynBioHub", "name of the blast db to use")

	synbiohubURL = flag.String("synbiohub.url", "https://synbiohub.org/sparql", "URL to send sparql queries to")
	resultLimit  = flag.Int("synbiohub.resultLimit", 100, "number of components to fetch in each query")

	redisURL          = flag.String("redis.url", "localhost:6379", "URL of redis instance storing dedup state")
	redisOffsetKey    = flag.String("redis.sequenceoffset", "sequenceoffset", "Redis key for max offset fetched from synbiohub")
	redisDedupSetKey  = flag.String("redis.sequenceHashSet", "sequenceHashSet", "Redis key for set storing all seen sequence hashes")
	redisSeqSetPrefix = flag.String("redis.sequencePrefix", "sequence",
		"Redis key prefix, appended with hash of sequence to store set of matching components")

	fastaDir = flag.String("fastas.path", "/var/synbioblast/fastas", "path to store fasta files in")
)

// I couldn't find a way to match an element with an attribute
// with a given value, otherwise we could parse directly
// into a []sequence
type sparqlResult struct {
	XMLName   xml.Name   `xml:"sparql"`
	Variables []variable `xml:"head>variable"`
	Results   []result   `xml:"results>result"`
}

type variable struct {
	Name string `xml:"name,attr"`
}

type result struct {
	Bindings []binding `xml:"binding"`
}

func (r *result) getValue(name string) string {
	for _, b := range r.Bindings {
		if b.Name == name {
			return b.Value
		}
	}

	return ""
}

type binding struct {
	Name     string `xml:"name,attr"`
	Value    string `xml:",any"`
	Datatype string `xml:",any,attr"`
}

type sequence struct {
	URI      string
	Sequence string
	Created  time.Time
}

func (s *sequence) Hash() string {
	sha := sha1.New()
	io.WriteString(sha, s.Sequence)
	return fmt.Sprintf("%x", sha.Sum(nil))
}

func parseSparqlTime(s string) (time.Time, error) {
	// this is way less complicated than I thought it would be
	return time.Parse(time.RFC3339, s)
}

func main() {
	flagfile.Load()

	log.Println("connecting to redis...")

	client, err := redis.Dial("tcp", *redisURL)
	if err != nil {
		log.Fatal("couldn't dial redis")
	}
	defer client.Close()

	offset, err := client.Cmd("GET", *redisOffsetKey).Int()
	// this block definitely isn't horrible /s
	if err != nil {
		if err == redis.ErrRespNil {
			err = client.Cmd("SET", *redisOffsetKey, 0).Err
			if err != nil {
				log.Fatal("couldn't set initial offset value")
			}
			log.Println("no offset val, setting it to 0")
			offset = 0
		} else {
			log.Fatal("couldn't get offset val: ", err)
		}
	} else {
		log.Printf("starting at offset %d", offset)
	}

	for {
		log.Println("fetching from virtuoso")

		bytes := fetch(offset)

		log.Println("fetched, parsing response...")

		seqs := parse(bytes)

		log.Println("fetched, processing")

		process(client, seqs)

		log.Printf("incrementing offset val by %d", len(seqs))

		offset, err = client.Cmd("INCRBY", *redisOffsetKey, len(seqs)).Int()
		if err != nil {
			log.Fatal("couldn't update offset with new records: ", err)
		}

		if len(seqs) < *resultLimit {
			log.Println("got less sequences than limit, sleeping")

			time.Sleep(time.Hour * 4)
		} else {
			log.Println("going again, but first sleeping for a bit...")

			time.Sleep(time.Second * 2)
		}
	}
}

func parse(bytes []byte) []sequence {
	result := &sparqlResult{}
	err := xml.Unmarshal(bytes, &result)
	if err != nil {
		log.Fatal("couldn't parse xml: ", err)
	}

	// TODO: check if result.variables is correct?

	sequences := make([]sequence, len(result.Results))
	for i, result := range result.Results {
		sequences[i].URI = result.getValue("uri")

		nucl := result.getValue("elements")
		sequences[i].Sequence = strings.ToLower(nucl)

		t, err := parseSparqlTime(result.getValue("created"))
		if err != nil {
			log.Fatal("couldn't parse time: ", result.getValue("created"))
		}
		sequences[i].Created = t
	}

	return sequences
}

func fetch(offset int) []byte {
	config := &queryParams{
		Limit:  *resultLimit,
		Offset: offset,
	}

	buf := bytes.NewBufferString(query)
	bank := sparql.LoadBank(buf)

	q, err := bank.Prepare("fetch", config)
	if err != nil {
		log.Fatal("couldn't prepare query: ", err)
	}

	vals := url.Values{}
	vals.Add("query", q)
	vals.Add("graph", "public")

	body := strings.NewReader(vals.Encode())

	req, err := http.NewRequest("POST", *synbiohubURL, body)
	if err != nil {
		log.Fatal("couldn't prepare request: ", err)
	}
	req.Header.Add("Accept", "*/*")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal("couldn't make request: ", err)
	}
	defer resp.Body.Close()

	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal("couldn't read xml: ", err)
	}

	return bytes
}

// TODO: transactions because we're like that?

func process(client *redis.Client, seqs []sequence) {
	for _, seq := range seqs {
		hash := seq.Hash()

		filename := path.Join(*fastaDir, hash+".fasta")

		file := []byte(fmt.Sprintf(">%s\n%s\n", hash, seq.Sequence))

		err := ioutil.WriteFile(filename, file, 0644)
		if err != nil {
			log.Fatal("couldn't write file "+filename+": ", err)
		}

		err = client.Cmd("SADD", *redisDedupSetKey, hash).Err
		if err != nil {
			log.Fatal("couldn't add hash to dedup set", err)
		}

		key := *redisSeqSetPrefix + ":" + hash
		err = client.Cmd("SADD", key, seq.URI).Err
		if err != nil {
			log.Fatal("couldn't add uri to sequence set: ", err)
		}
	}
}
