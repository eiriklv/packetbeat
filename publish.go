package main

import (
    "fmt"
    "labix.org/v2/mgo/bson"
    "github.com/mattbaird/elastigo/api"
    "github.com/mattbaird/elastigo/core"
    "encoding/json"
    "os"
    "time"
    "strings"
    "strconv"
)

type PublisherType struct {
    name         string

    url             string
    mother_host     string
    mother_port     string

    RefreshTopologyTimer <-chan time.Time
    TopologyMap map[string]string
}

var Publisher PublisherType

// Config
type tomlAgent struct {
    Name        string
    Refresh_topology_freq int
}
type tomlMothership struct {
    Host string
    Port int
}

type Event struct {
    Timestamp time.Time `json:"@timestamp"`
    Type string `json:"type"`
    Src_ip string `json:"src_ip"`
    Src_port uint16 `json:"src_port"`
    Src_proc string `json:"src_proc"`
    Src_country string `json:"src_country"`
    Src_server string `json:"src_server"`
    Dst_ip string `json:"dst_ip"`
    Dst_port uint16 `json:"dst_port"`
    Dst_proc string `json:"dst_proc"`
    Dst_server string `json:"dst_server"`
    ResponseTime int32 `json:"responsetime"`
    Status string `json:"status"`
    RequestRaw string `json:"request_raw"`
    ResponseRaw string `json:"response_raw"`

    Mysql bson.M `json:"mysql"`
    Http bson.M `json:"http"`
    Redis bson.M `json:"redis"`
}

type Topology struct {
    Name string `json:"name"`
    Ip string `json:"ip"`
}

func (publisher *PublisherType) GetServerName(ip string) string {
    // in case the IP is localhost, return current agent name
    islocal, err := IsLoopback(ip)
    if err != nil {
        ERR("Parsing IP %s fails with: %s", ip, err)
        return ""
    } else {
        if islocal {
            return publisher.name
        }
    }
    // find the agent with the desired IP
    name, exists := publisher.TopologyMap[ip]
    if !exists {
        return ""
    }
    return name
}

func (publisher *PublisherType) PublishHttpTransaction(t *HttpTransaction) error {
    // Set the Elasticsearch Host to Connect to
    api.Domain = publisher.mother_host
    api.Port = publisher.mother_port

    index := fmt.Sprintf("packetbeat-%d.%02d.%02d", t.ts.Year(), t.ts.Month(), t.ts.Day())

    status := t.Http["response"].(bson.M)["phrase"].(string)

    src_server := publisher.GetServerName(t.Src.Ip)
    dst_server := publisher.GetServerName(t.Dst.Ip)

    if dst_server != publisher.name {
        // duplicated transaction -> ignore it
        DEBUG("publish", "Ignore duplicated Http transaction on %s: %s -> %s", publisher.name, src_server, dst_server)
        return nil
    }

    var src_country = ""
    if _GeoLite != nil {
        if len(src_server) == 0 {   // only for external IP addresses
            loc := _GeoLite.GetLocationByIP(t.Src.Ip)
            if loc != nil {
                    src_country = loc.CountryCode
            }
        }
    }

    // add Http transaction
    _, err := core.Index(index, "http","", nil, Event{
        t.ts, "http", t.Src.Ip, t.Src.Port, t.Src.Proc, src_country, src_server,
        t.Dst.Ip, t.Dst.Port, t.Dst.Proc, dst_server,
	t.ResponseTime, status, t.Request_raw, t.Response_raw,
        nil, t.Http, nil})

    DEBUG("publish", "Sent Http transaction [%s->%s]:\n%s", t.Src.Proc, t.Dst.Proc, t.Http)
    return err

}

func (publisher *PublisherType) PublishMysqlTransaction(t *MysqlTransaction) error {
    // Set the Elasticsearch Host to Connect to
    api.Domain = publisher.mother_host
    api.Port = publisher.mother_port

    index := fmt.Sprintf("packetbeat-%d.%02d.%02d", t.ts.Year(), t.ts.Month(), t.ts.Day())

    status := t.Mysql["error_message"].(string)
    if len(status) == 0 {
        status = "OK"
    }

    src_server := publisher.GetServerName(t.Src.Ip)
    dst_server := publisher.GetServerName(t.Dst.Ip)

    // add Mysql transaction
    _, err := core.Index(index, "mysql", "", nil, Event{
        t.ts, "mysql", t.Src.Ip, t.Src.Port, t.Src.Proc, "", src_server,
        t.Dst.Ip, t.Dst.Port, t.Dst.Proc, dst_server,
	t.ResponseTime, status, t.Request_raw, t.Response_raw,
        t.Mysql, nil, nil})

    DEBUG("publish", "Sent MySQL transaction [%s->%s]:\n%s", t.Src.Proc, t.Dst.Proc, t.Mysql)

    return err

}

func (publisher *PublisherType) PublishRedisTransaction(t *RedisTransaction) error {
    // Set the Elasticsearch Host to Connect to
    api.Domain = publisher.mother_host
    api.Port = publisher.mother_port

    index := fmt.Sprintf("packetbeat-%d.%02d.%02d", t.ts.Year(), t.ts.Month(), t.ts.Day())

    status := "OK"

    src_server := publisher.GetServerName(t.Src.Ip)
    dst_server := publisher.GetServerName(t.Dst.Ip)

    // add Redis transaction
    _, err := core.Index(index, "redis","", nil, Event{
        t.ts, "redis", t.Src.Ip, t.Src.Port, t.Src.Proc, "", src_server,
        t.Dst.Ip, t.Dst.Port, t.Dst.Proc, dst_server,
	t.ResponseTime, status, t.Request_raw, t.Response_raw,
        nil, nil, t.Redis})

    DEBUG("publish", "Sent Redis transaction [%s->%s]:\n%s", t.Src.Proc, t.Dst.Proc, t.Redis)
    return err

}

func (publisher *PublisherType) UpdateTopologyPeriodically() {

    for _ = range publisher.RefreshTopologyTimer {
        publisher.UpdateTopology()
    }
}

func (publisher *PublisherType) UpdateTopology() {

    // Set the Elasticsearch Host to Connect to
    api.Domain = publisher.mother_host
    api.Port = publisher.mother_port

    DEBUG("publish", "Updating Topology")

    // get all agents IPs from Elasticsearch
    TopologyMapTmp := make(map[string]string)
    res, err := core.SearchUri("packetbeat-topology", "server-ip", nil)
    if err == nil {
        for _, server := range res.Hits.Hits {
            var top Topology
            err = json.Unmarshal([]byte(*server.Source), &top)
            if err != nil {
                ERR("json.Unmarshal fails with: %s", err)
            }
            // add mapping
            TopologyMapTmp[top.Ip] = top.Name
        }
    } else {
        ERR("core.SearchRequest fails with: %s", err)
    }

    // update topology map
    publisher.TopologyMap = TopologyMapTmp

    DEBUG("publish", "[%s] Map: %s", publisher.name, publisher.TopologyMap)
}

func (publisher *PublisherType) PublishTopology(params ...string) error {

    // Set the Elasticsearch Host to Connect to
    api.Domain = publisher.mother_host
    api.Port = publisher.mother_port

    var localAddrs []string = params

    if len(params) == 0 {
        addrs, err := LocalAddrs()
        if err != nil {
            ERR("Getting local IP addresses fails with: %s", err)
            return err
        }
        localAddrs = addrs
    }

    // delete old IP addresses
    searchJson := fmt.Sprintf("{query: {term: {name: %s}}}",strconv.Quote(publisher.name))
    res, err := core.SearchRequest("packetbeat-topology", "server-ip", nil, searchJson)
    if err == nil  {
        for _, server := range res.Hits.Hits {

            var top Topology
            err = json.Unmarshal([]byte(*server.Source), &top)
            if err != nil {
                ERR("Failed to unmarshal json data: %s", err)
            }
            if !stringInSlice(top.Ip, localAddrs) {
                res, err := core.Delete("packetbeat-topology", "server-ip",/*id*/top.Ip,  nil)
                if err != nil {
                    ERR("Failed to delete the old IP address from packetbeat-topology")
                }
                if !res.Ok {
                    ERR("Fail to delete old topology entry")
                }
            }

        }
    }

    // add new IP addresses
    for _, addr := range localAddrs {

        // check if the IP is already in the elasticsearch, before adding it 
        found, err := core.Exists("packetbeat-topology", "server-ip", /*id*/addr, nil)
        if err != nil {
            ERR("core.Exists fails with: %s", err)
        } else {

            if !found {
                res, err := core.Index("packetbeat-topology", "server-ip", /*id*/addr, nil,
                    Topology{publisher.name, addr})
                if err != nil {
                    return err
                }
                if !res.Ok {
                    ERR("Fail to add new topology entry")
                }
            }
        }
    }

    DEBUG("publish", "Topology: name=%s, ips=%s", publisher.name, strings.Join(localAddrs, " "))

    // initialize local topology map
    publisher.TopologyMap = make(map[string]string)

    return nil
}

func (publisher *PublisherType) Init() error {
    var err error

    publisher.mother_host = _Config.Elasticsearch.Host
    publisher.mother_port = fmt.Sprintf("%d", _Config.Elasticsearch.Port)

    publisher.url = fmt.Sprintf("%s:%s", publisher.mother_host, publisher.mother_port)
    INFO("Use %s as publisher", publisher.url)

    publisher.name = _Config.Agent.Name
    if len(publisher.name) == 0 {
        // use the hostname
        publisher.name, err = os.Hostname()
        if err != nil {
            return err
        }

        INFO("No agent name configured, using hostname '%s'", publisher.name)
    }

    RefreshTopologyFreq := 10 * time.Second
    if _Config.Agent.Refresh_topology_freq != 0 {
        RefreshTopologyFreq = time.Duration(_Config.Agent.Refresh_topology_freq) * time.Second
    }
    publisher.RefreshTopologyTimer = time.Tick( RefreshTopologyFreq )

    // register agent and its public IP addresses
    err = publisher.PublishTopology()
    if err != nil {
        ERR("Failed to publish topology: %s", err)
        return err
    }
    // update topology periodically

    go publisher.UpdateTopologyPeriodically()

    return nil
}
