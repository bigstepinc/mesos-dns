// Package records contains functions to generate resource records from
// mesos master states to serve through a dns server
package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/mesosphere/mesos-dns/logging"
	"github.com/mesosphere/mesos-dns/records/labels"
	"github.com/mesosphere/mesos-dns/records/state"
	"github.com/mesosphere/mesos-dns/records/tmpl"
)

// Map host/service name to DNS answer
// REFACTOR - when discoveryinfo is integrated
// Will likely become map[string][]discoveryinfo
type rrs map[string][]string

// RecordGenerator contains DNS records and methods to access and manipulate
// them. TODO(kozyraki): Refactor when discovery id is available.
type RecordGenerator struct {
	As       rrs
	SRVs     rrs
	SlaveIPs map[string]string
}

// ParseState retrieves and parses the Mesos master /state.json and converts it
// into DNS records.
func (rg *RecordGenerator) ParseState(leader string, c Config) error {

	// find master -- return if error
	sj, err := rg.findMaster(leader, c.Masters)
	if err != nil {
		logging.Error.Println("no master")
		return err
	}
	if sj.Leader == "" {
		logging.Error.Println("Unexpected error")
		err = errors.New("empty master")
		return err
	}

	hostSpec := labels.RFC1123
	if c.EnforceRFC952 {
		hostSpec = labels.RFC952
	}

	return rg.InsertState(sj, c.Domain, c.SOARname, c.Listener, c.Masters, hostSpec, c.Templates)
}

// Tries each master and looks for the leader
// if no leader responds it errors
func (rg *RecordGenerator) findMaster(leader string, masters []string) (state.State, error) {
	var sj state.State

	// Check if ZK master is correct
	if leader != "" {
		logging.VeryVerbose.Println("Zookeeper says the leader is: ", leader)
		ip, port, err := getProto(leader)
		if err != nil {
			logging.Error.Println(err)
		}

		sj, _ = rg.loadWrap(ip, port)
		if sj.Leader != "" {
			return sj, nil
		}
		logging.Verbose.Println("Warning: Zookeeper is wrong about leader")
		if len(masters) == 0 {
			return sj, errors.New("no master")
		}
		logging.Verbose.Println("Warning: falling back to Masters config field: ", masters)
	}

	// try each listed mesos master before dying
	for i, master := range masters {
		ip, port, err := getProto(master)
		if err != nil {
			logging.Error.Println(err)
		}

		sj, _ = rg.loadWrap(ip, port)
		if sj.Leader == "" {
			logging.VeryVerbose.Println("Warning: not a leader - trying next one")
			if len(masters)-1 == i {
				return sj, errors.New("no master")
			}

		} else {
			return sj, nil
		}

	}

	return sj, errors.New("no master")
}

// Loads state.json from mesos master
func (rg *RecordGenerator) loadFromMaster(ip string, port string) (sj state.State) {
	// REFACTOR: state.json security
	url := "http://" + ip + ":" + port + "/master/state.json"

	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		logging.Error.Println(err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logging.Error.Println(err)
	}
	_ = resp.Body.Close()

	err = json.Unmarshal(body, &sj)
	if err != nil {
		logging.Error.Println(err)
	}

	return sj
}

// Catches an attempt to load state.json from a mesos master
// attempts can fail from down server or mesos master secondary
// it also reloads from a different master if the master it attempted to
// load from was not the leader
func (rg *RecordGenerator) loadWrap(ip string, port string) (state.State, error) {
	var err error
	var sj state.State

	defer func() {
		if rec := recover(); rec != nil {
			err = errors.New("can't connect to master")
		}

	}()

	logging.VeryVerbose.Println("reloading from master " + ip)
	sj = rg.loadFromMaster(ip, port)

	if rip := leaderIP(sj.Leader); rip != ip {
		logging.VeryVerbose.Println("Warning: master changed to " + ip)
		sj = rg.loadFromMaster(rip, port)
	}

	return sj, err
}

// attempt to translate the hostname into an IPv4 address. logs an error if IP
// lookup fails. if an IP address cannot be found, returns the same hostname
// that was given. upon success returns the IP address as a string.
func hostToIP4(hostname string) (string, bool) {
	ip := net.ParseIP(hostname)
	if ip == nil {
		t, err := net.ResolveIPAddr("ip4", hostname)
		if err != nil {
			logging.Error.Printf("cannot translate hostname %q into an ip4 address", hostname)
			return hostname, false
		}
		ip = t.IP
	}
	return ip.String(), true
}

func compileNonCanonicalTemplates(ts []tmpl.Template, spec labels.Func) []*tmpl.Compiled {
	compiled := make([]*tmpl.Compiled, 0, len(ts))
	for _, t := range ts {
		c, err := t.Compile(spec)
		if err != nil {
			logging.Error.Println(err)
			continue
		}
		compiled = append(compiled, c)
	}
	return compiled
}

func compileEssentialTemplates(t tmpl.Template, spec labels.Func) *tmpl.Compiled {
	compiled, err := t.Compile(spec)
	if err != nil {
		logging.Error.Fatalf("cannot compile invalid template %q: %v", t, err)
	}
	return compiled
}

// InsertState transforms a StateJSON into RecordGenerator RRs
func (rg *RecordGenerator) InsertState(sj state.State, domain, ns, listener string,
	masters []string, spec labels.Func, templates []tmpl.Template) error {

	rg.SlaveIPs = map[string]string{}
	rg.SRVs = rrs{}
	rg.As = rrs{}
	rg.frameworkRecords(sj, domain, spec)
	rg.slaveRecords(sj, domain, spec)
	rg.listenerRecord(listener, ns)
	rg.masterRecord(domain, masters, sj.Leader)
	rg.taskRecords(sj, domain, spec, templates)

	return nil
}

// frameworkRecords injects A and SRV records into the generator store:
//     frameworkname.domain.                 // resolves to IPs of each framework
//     _framework._tcp.frameworkname.domain. // resolves to the driver port and IP of each framework
func (rg *RecordGenerator) frameworkRecords(sj state.State, domain string, spec labels.Func) {
	for _, f := range sj.Frameworks {
		fname := labels.DomainFrag(f.Name, labels.Sep, spec)
		host, port := f.HostPort()
		if address, ok := hostToIP4(host); ok {
			a := fname + "." + domain + "."
			rg.insertRR(a, address, "A")
			if port != "" {
				srv := net.JoinHostPort(a, port)
				rg.insertRR("_framework._tcp."+a, srv, "SRV")
			}
		}
	}
}

// slaveRecords injects A and SRV records into the generator store:
//     slave.domain.      // resolves to IPs of all slaves
//     _slave._tc.domain. // resolves to the driver port and IP of all slaves
func (rg *RecordGenerator) slaveRecords(sj state.State, domain string, spec labels.Func) {
	for _, slave := range sj.Slaves {
		address, ok := hostToIP4(slave.PID.Host)
		if ok {
			a := "slave." + domain + "."
			rg.insertRR(a, address, "A")
			srv := net.JoinHostPort(a, slave.PID.Port)
			rg.insertRR("_slave._tcp."+domain+".", srv, "SRV")
		} else {
			logging.VeryVerbose.Printf("string '%q' for slave with id %q is not a valid IP address", address, slave.ID)
			address = labels.DomainFrag(address, labels.Sep, spec)
		}
		rg.SlaveIPs[slave.ID] = address
	}
}

// masterRecord injects A and SRV records into the generator store:
//     master.domain.  // resolves to IPs of all masters
//     masterN.domain. // one IP address for each master
//     leader.domain.  // one IP address for the leading master
//
// The current func implementation makes an assumption about the order of masters:
// it's the order in which you expect the enumerated masterN records to be created.
// This is probably important: if a new leader is elected, you may not want it to
// become master0 simply because it's the leader. You probably want your DNS records
// to change as little as possible. And this func should have the least impact on
// enumeration order, or name/IP mappings - it's just creating the records. So let
// the caller do the work of ordering/sorting (if desired) the masters list if a
// different outcome is desired.
//
// Another consequence of the current overall mesos-dns app implementation is that
// the leader may not even be in the masters list at some point in time. masters is
// really fallback-masters (only consider these to be masters if I can't find a
// leader via ZK). At some point in time, they may not actually be masters any more.
// Consider a cluster of 3 nodes that suffers the loss of a member, and gains a new
// member (VM crashed, was replaced by another VM). And the cycle repeats several
// times. You end up with a set of running masters (and leader) that's different
// than the set of statically configured fallback masters.
//
// So the func tries to index the masters as they're listed and begrudgingly assigns
// the leading master an index out-of-band if it's not actually listed in the masters
// list. There are probably better ways to do it.
func (rg *RecordGenerator) masterRecord(domain string, masters []string, leader string) {
	// create records for leader
	// A records
	h := strings.Split(leader, "@")
	if len(h) < 2 {
		logging.Error.Println(leader)
		return // avoid a panic later
	}
	leaderAddress := h[1]
	ip, port, err := getProto(leaderAddress)
	if err != nil {
		logging.Error.Println(err)
		return
	}
	arec := "leader." + domain + "."
	rg.insertRR(arec, ip, "A")
	arec = "master." + domain + "."
	rg.insertRR(arec, ip, "A")

	// SRV records
	tcp := "_leader._tcp." + domain + "."
	udp := "_leader._udp." + domain + "."
	host := "leader." + domain + "." + ":" + port
	rg.insertRR(tcp, host, "SRV")
	rg.insertRR(udp, host, "SRV")

	// if there is a list of masters, insert that as well
	addedLeaderMasterN := false
	idx := 0
	for _, master := range masters {

		ip, _, err := getProto(master)
		if err != nil {
			logging.Error.Println(err)
			continue
		}

		// A records (master and masterN)
		if master != leaderAddress {
			arec := "master." + domain + "."
			added := rg.insertRR(arec, ip, "A")
			if !added {
				// duplicate master?!
				continue
			}
		}

		if master == leaderAddress && addedLeaderMasterN {
			// duplicate leader in masters list?!
			continue
		}

		arec := "master" + strconv.Itoa(idx) + "." + domain + "."
		rg.insertRR(arec, ip, "A")
		idx++

		if master == leaderAddress {
			addedLeaderMasterN = true
		}
	}
	// flake: we ended up with a leader that's not in the list of all masters?
	if !addedLeaderMasterN {
		// only a flake if there were fallback masters configured
		if len(masters) > 0 {
			logging.Error.Printf("warning: leader %q is not in master list", leader)
		}
		arec = "master" + strconv.Itoa(idx) + "." + domain + "."
		rg.insertRR(arec, ip, "A")
	}
}

// A record for mesos-dns (the name is listed in SOA replies)
func (rg *RecordGenerator) listenerRecord(listener string, ns string) {
	if listener == "0.0.0.0" {
		rg.setFromLocal(listener, ns)
	} else if listener == "127.0.0.1" {
		rg.insertRR(ns, "127.0.0.1", "A")
	} else {
		rg.insertRR(ns, listener, "A")
	}
}

func (rg *RecordGenerator) taskRecords(sj state.State, domain string, spec labels.Func,
	templates []tmpl.Template) {
	// pre-compile the tmpl. Only do this once before all the records.
	compiledTemplates := compileNonCanonicalTemplates(templates, spec)
	canonicalTemplate := compileEssentialTemplates("{name}-{task-id-hash}-{slave-id-short}.{framework}", spec)
	tcpRFC2782Template := compileEssentialTemplates("_{name}._tcp.{framework}", spec)
	udpRFC2782Template := compileEssentialTemplates("_{name}._udp.{framework}", spec)

	// complete crap - refactor me
	for _, f := range sj.Frameworks {
		// insert taks records
		fname := labels.DomainFrag(f.Name, labels.Sep, spec)
		for _, task := range f.Tasks {
			hostIP, ok := rg.SlaveIPs[task.SlaveID]

			// skip not running or not discoverable tasks
			if !ok || (task.State != "TASK_RUNNING") {
				continue
			}

			// context used to build domain names
			ctx := tmpl.NewContext(&task, fname, spec)

			// insert canonical A records
			trec, err := canonicalTemplate.Execute(ctx, domain)
			if err != nil {
				logging.Error.Printf("error creating the canonical host name for task %v: %v", task, err)
				continue
			}

			containerIP := task.ContainerIP()
			rg.insertRR(trec, hostIP, "A")
			if containerIP != "" {
				rg.insertRR("_container."+trec, containerIP, "A")
			}

			// insert canonical SRV records
			for _, port := range task.Ports() {
				rg.insertRR(trec, hostIP+":"+port, "SRV")
				if containerIP != "" {
					rg.insertRR("_container."+trec, containerIP+":"+port, "SRV")
				}
			}

			// insert custom records
			for _, cp := range compiledTemplates {
				host, err := cp.Execute(ctx, domain)
				if err != nil {
					logging.VeryVerbose.Printf("failed to template %q with context %v: %v", cp.Template, ctx, err)
					continue
				}

				// insert A recordsbaseName
				rg.insertRR(host, hostIP, "A")
				if containerIP != "" {
					rg.insertRR("_container."+host, containerIP, "A")
				}

				// insert SRV records
				for _, port := range task.Ports() {
					srvHost := trec + ":" + port
					rg.insertRR(host, srvHost, "SRV")
					if containerIP != "" {
						rg.insertRR("_container."+host, srvHost, "SRV")
					}
				}
			}

			// Add RFC 2782 SRV records
			for _, port := range task.Ports() {
				srvHost := trec + ":" + port
				tcp, _ := tcpRFC2782Template.Execute(ctx, domain)
				udp, _ := udpRFC2782Template.Execute(ctx, domain)
				rg.insertRR(tcp, srvHost, "SRV")
				rg.insertRR(udp, srvHost, "SRV")
			}
		}
	}
}

// A records for each local interface
// If this causes problems you should explicitly set the
// listener address in config.json
func (rg *RecordGenerator) setFromLocal(host string, ns string) {

	ifaces, err := net.Interfaces()
	if err != nil {
		logging.Error.Println(err)
	}

	// handle err
	for _, i := range ifaces {

		addrs, err := i.Addrs()
		if err != nil {
			logging.Error.Println(err)
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue
			}

			rg.insertRR(ns, ip.String(), "A")
		}
	}
}

func (rg *RecordGenerator) exists(name, host, rtype string) bool {
	if rtype == "A" {
		if val, ok := rg.As[name]; ok {
			// check if A record already exists
			// identical tasks on same slave
			for _, b := range val {
				if b == host {
					return true
				}
			}
		}
	} else {
		if val, ok := rg.SRVs[name]; ok {
			// check if SRV record already exists
			for _, b := range val {
				if b == host {
					return true
				}
			}
		}
	}
	return false
}

// insertRR adds a record to the appropriate record map for the given name/host pair,
// but only if the pair is unique. returns true if added, false otherwise.
// TODO(???): REFACTOR when storage is updated
func (rg *RecordGenerator) insertRR(name, host, rtype string) bool {
	logging.VeryVerbose.Println("[" + rtype + "]\t" + name + ": " + host)

	if rg.exists(name, host, rtype) {
		return false
	}
	if rtype == "A" {
		val := rg.As[name]
		rg.As[name] = append(val, host)
	} else {
		val := rg.SRVs[name]
		rg.SRVs[name] = append(val, host)
	}
	return true
}

// leaderIP returns the ip for the mesos master
// input format master@ip:port
func leaderIP(leader string) string {
	pair := strings.Split(leader, "@")[1]
	return strings.Split(pair, ":")[0]
}

// should be able to accept
// ip:port
// zk://host1:port1,host2:port2,.../path
// zk://username:password@host1:port1,host2:port2,.../path
// file:///path/to/file (where file contains one of the above)
func getProto(pair string) (string, string, error) {
	h := strings.SplitN(pair, ":", 2)
	if len(h) != 2 {
		return "", "", fmt.Errorf("unable to parse proto from %q", pair)
	}
	return h[0], h[1], nil
}
