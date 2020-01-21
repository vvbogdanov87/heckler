package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"text/template"
	"time"

	"./gitutil"
	"./puppetutil"
	"github.com/Masterminds/sprig"
	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/github"
	git "github.com/libgit2/git2go"
	"google.golang.org/grpc"
)

var Debug = false
var RegexDefineType = regexp.MustCompile(`^[A-Z][a-zA-Z0-9_:]*\[[^\]]+\]$`)

const GitHubEnterpriseURL = "https://github.braintreeps.com/api/v3"

type hostFlags []string

type Node struct {
	host                 string
	commitReports        map[git.Oid]*puppetutil.PuppetReport
	commitDeltaResources map[git.Oid]map[string]*deltaResource
	rizzoClient          puppetutil.RizzoClient
}

type deltaResource struct {
	Title      string
	Type       string
	DefineType string
	Events     []*puppetutil.Event
	Logs       []*puppetutil.Log
}

type groupResource struct {
	Title      string
	Type       string
	DefineType string
	Diff       string
	Nodes      []string
	Events     []*groupEvent
	Logs       []*groupLog
}

type groupEvent struct {
	PreviousValue string
	DesiredValue  string
}

type groupLog struct {
	Level   string
	Message string
}

func prettyPrint(i interface{}) string {
	s, _ := json.MarshalIndent(i, "", "\t")
	return string(s)
}

func normalizeLogs(Logs []*puppetutil.Log) []*puppetutil.Log {
	var newSource string
	var origSource string
	var newLogs []*puppetutil.Log

	// extract resource from log source
	regexResourcePropertyTail := regexp.MustCompile(`/[a-z][a-z0-9_]*$`)
	regexResourceTail := regexp.MustCompile(`[^\/]+\[[^\[\]]+\]$`)

	// normalize diff
	reFileContent := regexp.MustCompile(`File\[.*content$`)
	reDiff := regexp.MustCompile(`(?s)^.---`)

	// Log referring to a puppet resource
	regexResource := regexp.MustCompile(`^/Stage`)

	// Log msg values to drop
	regexCurValMsg := regexp.MustCompile(`^current_value`)
	regexApplyMsg := regexp.MustCompile(`^Applied catalog`)
	regexRefreshMsg := regexp.MustCompile(`^Would have triggered 'refresh'`)

	// Log sources to drop
	regexClass := regexp.MustCompile(`^Class\[`)
	regexStage := regexp.MustCompile(`^Stage\[`)

	for _, l := range Logs {
		origSource = ""
		newSource = ""
		if regexCurValMsg.MatchString(l.Message) ||
			regexApplyMsg.MatchString(l.Message) {
			if Debug {
				fmt.Fprintf(os.Stderr, "Dropping Log: %v: %v\n", l.Source, l.Message)
			}
			continue
		} else if regexClass.MatchString(l.Source) ||
			regexStage.MatchString(l.Source) ||
			RegexDefineType.MatchString(l.Source) {
			if Debug {
				fmt.Fprintf(os.Stderr, "Dropping Log: %v: %v\n", l.Source, l.Message)
			}
			continue
		} else if (!regexResource.MatchString(l.Source)) && regexRefreshMsg.MatchString(l.Message) {
			if Debug {
				fmt.Fprintf(os.Stderr, "Dropping Log: %v: %v\n", l.Source, l.Message)
			}
			continue
		} else if regexResource.MatchString(l.Source) {
			origSource = l.Source
			newSource = regexResourcePropertyTail.ReplaceAllString(l.Source, "")
			newSource = regexResourceTail.FindString(newSource)
			if newSource == "" {
				fmt.Fprintf(os.Stderr, "newSource is empty!\n")
				fmt.Fprintf(os.Stderr, "Log: '%v' -> '%v': %v\n", origSource, newSource, l.Message)
				os.Exit(1)
			}

			if reFileContent.MatchString(l.Source) && reDiff.MatchString(l.Message) {
				l.Message = normalizeDiff(l.Message)
			}
			l.Source = newSource
			if Debug {
				fmt.Fprintf(os.Stderr, "Adding Log: '%v' -> '%v': %v\n", origSource, newSource, l.Message)
			}
			newLogs = append(newLogs, l)
		} else {
			fmt.Fprintf(os.Stderr, "Unaccounted for Log: %v: %v\n", l.Source, l.Message)
			newLogs = append(newLogs, l)
		}
	}

	return newLogs
}

func normalizeDiff(msg string) string {
	var newMsg string
	var s string
	var line int

	scanner := bufio.NewScanner(strings.NewReader(msg))
	line = 0
	for scanner.Scan() {
		s = scanner.Text()
		if line > 2 {
			newMsg += s + "\n"
		}
		line++
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	return newMsg
}

func commitToMarkdown(c *git.Commit) string {
	var body strings.Builder
	var err error

	tpl := template.Must(template.New("base").Funcs(sprig.TxtFuncMap()).ParseGlob("*.tmpl"))

	err = tpl.ExecuteTemplate(&body, "commit.tmpl", c)
	if err != nil {
		log.Fatal(err)
	}
	return body.String()
}

func groupResourcesToMarkdown(groupedResources []*groupResource) string {
	var body strings.Builder
	var err error

	tpl := template.Must(template.New("base").Funcs(sprig.TxtFuncMap()).ParseGlob("*.tmpl"))

	err = tpl.ExecuteTemplate(&body, "groupResource.tmpl", groupedResources)
	if err != nil {
		log.Fatal(err)
	}
	return body.String()
}

func deltaNoop(priorCommitNoop *puppetutil.PuppetReport, commitNoop *puppetutil.PuppetReport) map[string]*deltaResource {
	var foundPrior bool
	var deltaEvents []*puppetutil.Event
	var deltaLogs []*puppetutil.Log
	var dr map[string]*deltaResource
	var partOfDefine bool
	var defineType string

	dr = make(map[string]*deltaResource)

	for resourceTitle, r := range commitNoop.ResourceStatuses {
		partOfDefine = false
		deltaEvents = nil
		deltaLogs = nil
		defineType = ""

		cplen := len(r.ContainmentPath)
		if cplen > 2 {
			possibleDefineType := r.ContainmentPath[cplen-2]
			if RegexDefineType.MatchString(possibleDefineType) {
				partOfDefine = true
				defineType = possibleDefineType
			}
		}

		for _, e := range r.Events {
			if priorResourceStatuses, ok := priorCommitNoop.ResourceStatuses[resourceTitle]; ok {
				foundPrior = false
				for _, pe := range priorResourceStatuses.Events {
					if *e == *pe {
						foundPrior = true
						break
					}
				}
				if foundPrior == false {
					deltaEvents = append(deltaEvents, e)
				}
			} else {
				// no prior events at all, so no need to compare
				deltaEvents = append(deltaEvents, e)
			}
		}

		for _, l := range commitNoop.Logs {
			if l.Source == resourceTitle {
				foundPrior = false
				for _, pl := range priorCommitNoop.Logs {
					if *l == *pl {
						foundPrior = true
						break
					}
				}
				if foundPrior == false {
					deltaLogs = append(deltaLogs, l)
				}
			}
		}

		if len(deltaEvents) > 0 || len(deltaLogs) > 0 {
			dr[resourceTitle] = new(deltaResource)
			dr[resourceTitle].Title = resourceTitle
			dr[resourceTitle].Type = r.ResourceType
			dr[resourceTitle].Events = deltaEvents
			dr[resourceTitle].Logs = deltaLogs
			if partOfDefine {
				dr[resourceTitle].DefineType = defineType
			}
		}
	}

	return dr
}

func groupResources(commitId git.Oid, targetDeltaResource *deltaResource, nodes map[string]*Node, groupedCommits map[git.Oid][]*groupResource) {
	var nodeList []string
	var desiredValue string
	// XXX Remove this hack, only needed for old versions of puppet 4.5?
	var regexRubySym = regexp.MustCompile(`^:`)
	var gr *groupResource
	var ge *groupEvent
	var gl *groupLog

	for nodeName, node := range nodes {
		if nodeDeltaResource, ok := node.commitDeltaResources[commitId][targetDeltaResource.Title]; ok {
			// fmt.Printf("grouping %v\n", targetDeltaResource.Title)
			if cmp.Equal(targetDeltaResource, nodeDeltaResource) {
				nodeList = append(nodeList, nodeName)
				delete(node.commitDeltaResources[commitId], targetDeltaResource.Title)
			} else {
				// fmt.Printf("Diff:\n %v", cmp.Diff(targetDeltaResource, nodeDeltaResource))
			}
		}
	}

	gr = new(groupResource)
	gr.Title = targetDeltaResource.Title
	gr.Type = targetDeltaResource.Type
	gr.DefineType = targetDeltaResource.DefineType
	sort.Strings(nodeList)
	gr.Nodes = nodeList

	for _, e := range targetDeltaResource.Events {
		ge = new(groupEvent)

		ge.PreviousValue = regexRubySym.ReplaceAllString(e.PreviousValue, "")
		// XXX move base64 decode somewhere else
		// also yell at puppet for this inconsistency!!!
		if targetDeltaResource.Type == "File" && e.Property == "content" {
			data, err := base64.StdEncoding.DecodeString(e.DesiredValue)
			if err != nil {
				// XXX nasty, fix?
				desiredValue = e.DesiredValue
			} else {
				desiredValue = string(data[:])
			}
		} else {
			desiredValue = regexRubySym.ReplaceAllString(e.DesiredValue, "")
		}
		ge.DesiredValue = desiredValue
		gr.Events = append(gr.Events, ge)
	}
	regexDiff := regexp.MustCompile(`^@@ `)
	for _, l := range targetDeltaResource.Logs {
		if regexDiff.MatchString(l.Message) {
			gr.Diff = strings.TrimSuffix(l.Message, "\n")
		} else {

			gl = new(groupLog)
			gl.Level = l.Level
			gl.Message = strings.TrimRight(l.Message, "\n")
			gr.Logs = append(gr.Logs, gl)
		}
	}
	groupedCommits[commitId] = append(groupedCommits[commitId], gr)
}

func (i *hostFlags) String() string {
	return fmt.Sprint(*i)
}

func (i *hostFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func fetchRepo() (*git.Repository, error) {

	// Shared transport to reuse TCP connections.
	tr := http.DefaultTransport

	// Wrap the shared transport for use with the app ID 7 authenticating with
	// installation ID 11.
	itr, err := ghinstallation.NewKeyFromFile(tr, 7, 11, "heckler.2019-10-30.private-key.pem")
	if err != nil {
		return nil, err
	}
	itr.BaseURL = GitHubEnterpriseURL

	// Use installation transport with github.com/google/go-github
	_, err = github.NewEnterpriseClient(GitHubEnterpriseURL, GitHubEnterpriseURL, &http.Client{Transport: itr})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	tok, err := itr.Token(ctx)
	if err != nil {
		return nil, err
	}

	cloneDir := "/data/muppetshow"
	cloneOptions := &git.CloneOptions{}
	remoteUrl := fmt.Sprintf("https://x-access-token:%s@github.braintreeps.com/lollipopman/muppetshow", tok)
	repo, err := gitutil.CloneOrOpen(remoteUrl, cloneDir, cloneOptions)
	if err != nil {
		return nil, err
	}
	err = gitutil.FastForward(repo, nil)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func hecklerApply(rc puppetutil.RizzoClient, c chan<- puppetutil.PuppetReport, par puppetutil.PuppetApplyRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*300)
	defer cancel()
	r, err := rc.PuppetApply(ctx, &par)
	if err != nil {
		c <- puppetutil.PuppetReport{}
	}
	c <- *r
}

func grpcConnect(node *Node, clientConnChan chan *Node) {
	var conn *grpc.ClientConn
	address := node.host + ":50051"
	log.Printf("Dialing: %v", node.host)
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatalf("Unable to connect to: %v, %v", node.host, err)
	}
	log.Printf("Connected: %v", node.host)
	node.rizzoClient = puppetutil.NewRizzoClient(conn)
	clientConnChan <- node
}

func commitIdList(repo *git.Repository, beginRev string, endRev string) ([]git.Oid, error) {
	var commitIds []git.Oid

	log.Printf("Walk begun: %s..%s\n", beginRev, endRev)
	rv, err := repo.Walk()
	if err != nil {
		return []git.Oid{}, err
	}

	rv.Sorting(git.SortTopological)

	// XXX only tags???
	err = rv.PushRef("refs/tags/" + endRev)
	if err != nil {
		return []git.Oid{}, err
	}
	err = rv.HideRef("refs/tags/" + beginRev)
	if err != nil {
		return []git.Oid{}, err
	}

	var gi git.Oid
	for rv.Next(&gi) == nil {
		commitIds = append([]git.Oid{gi}, commitIds...)
	}
	log.Printf("Walk Successful\n")

	return commitIds, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	var hosts hostFlags
	var beginRev string
	var endRev string
	var rev string
	var noop bool
	var data []byte
	var nodes map[string]*Node
	var puppetReportChan chan puppetutil.PuppetReport
	var node *Node

	puppetReportChan = make(chan puppetutil.PuppetReport)

	var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to `file`")
	var memprofile = flag.String("memprofile", "", "write memory profile to `file`")
	flag.Var(&hosts, "node", "node hostnames to group")
	flag.StringVar(&beginRev, "beginrev", "", "begin rev")
	flag.StringVar(&endRev, "endrev", "", "end rev")
	flag.StringVar(&rev, "rev", "", "rev to apply or noop")
	flag.BoolVar(&noop, "noop", false, "noop")
	flag.BoolVar(&Debug, "debug", false, "enable debugging")
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	if rev != "" && (beginRev != "" || endRev != "") {
		fmt.Printf("The -rev flag cannot be combined with the -beginrev or the -endrev\n")
		flag.Usage()
		os.Exit(1)
	}

	if len(hosts) == 0 {
		fmt.Printf("ERROR: You must supply one or more nodes\n")
		flag.Usage()
		os.Exit(1)
	}

	repo, err := fetchRepo()
	if err != nil {
		log.Fatalf("Unable to fetch repo: %v", err)
	}

	var clientConnChan chan *Node
	clientConnChan = make(chan *Node)

	nodes = make(map[string]*Node)
	for _, host := range hosts {
		nodes[host] = new(Node)
		nodes[host].host = host
	}

	for _, node := range nodes {
		go grpcConnect(node, clientConnChan)
	}

	for range nodes {
		node = <-clientConnChan
		log.Printf("Conn %s\n", node.host)
		nodes[node.host] = node
	}

	if rev != "" {
		par := puppetutil.PuppetApplyRequest{Rev: rev, Noop: noop}
		for _, node := range nodes {
			go hecklerApply(node.rizzoClient, puppetReportChan, par)
		}

		for range hosts {
			r := <-puppetReportChan
			log.Printf("Applied: %s@%s", r.Host, r.ConfigurationVersion)
		}
		os.Exit(0)
	}

	if beginRev == "" || endRev == "" {
		fmt.Printf("ERROR: You must supply -beginrev & -endrev or -rev\n")
		flag.Usage()
		os.Exit(1)
	}

	// Make dir structure
	// e.g. /var/heckler/v1..v2//oid.json

	revdir := fmt.Sprintf("/var/heckler/%s..%s", beginRev, endRev)

	os.MkdirAll(revdir, 077)
	for host, _ := range nodes {
		os.Mkdir(revdir+"/"+host, 077)
	}

	var groupedCommits map[git.Oid][]*groupResource

	groupedCommits = make(map[git.Oid][]*groupResource)

	// XXX Should or can this be done in new(Node)?
	for _, node := range nodes {
		node.commitReports = make(map[git.Oid]*puppetutil.PuppetReport)
		node.commitDeltaResources = make(map[git.Oid]map[string]*deltaResource)
	}

	commitIds, err := commitIdList(repo, beginRev, endRev)
	if err != nil {
		log.Fatalf("Unable to get commit id list", err)
	}

	var noopRequests int
	var reportPath string
	var file *os.File
	var rprt *puppetutil.PuppetReport

	for i, commitId := range commitIds {
		log.Printf("Nooping: %s (%d of %d)", commitId.String(), i, len(commitIds))
		par := puppetutil.PuppetApplyRequest{Rev: commitId.String(), Noop: true}
		noopRequests = 0
		for host, node := range nodes {
			reportPath = revdir + "/" + host + "/" + commitId.String() + ".json"
			if _, err := os.Stat(reportPath); err == nil {
				file, err = os.Open(reportPath)
				if err != nil {
					log.Fatal(err)
				}
				defer file.Close()

				data, err = ioutil.ReadAll(file)
				if err != nil {
					log.Fatalf("cannot read report: %v", err)
				}
				rprt = new(puppetutil.PuppetReport)
				err = json.Unmarshal([]byte(data), rprt)
				if err != nil {
					log.Fatalf("cannot unmarshal report: %v", err)
				}
				if host != rprt.Host {
					log.Fatalf("Host mismatch %s != %s", host, rprt.Host)
				}
				log.Printf("Found serialized noop: %s@%s", rprt.Host, rprt.ConfigurationVersion)
				nodes[rprt.Host].commitReports[commitId] = rprt
			} else {
				go hecklerApply(node.rizzoClient, puppetReportChan, par)
				noopRequests++
			}
		}

		for j := 0; j < noopRequests; j++ {
			rprt := <-puppetReportChan
			log.Printf("Received noop: %s@%s", rprt.Host, rprt.ConfigurationVersion)
			nodes[rprt.Host].commitReports[commitId] = &rprt
			nodes[rprt.Host].commitReports[commitId].Logs = normalizeLogs(nodes[rprt.Host].commitReports[commitId].Logs)

			reportPath = revdir + "/" + rprt.Host + "/" + commitId.String() + ".json"
			data, err = json.Marshal(rprt)
			if err != nil {
				log.Fatalf("Cannot marshal report: %v", err)
			}
			err = ioutil.WriteFile(reportPath, data, 0644)
			if err != nil {
				log.Fatalf("Cannot write report: %v", err)
			}

		}

		for host, node := range nodes {
			if i > 0 {
				log.Printf("Creating delta resource: %s@(%s - %s)", host, commitId.String(), commitIds[i-1].String())
				node.commitDeltaResources[commitId] = deltaNoop(node.commitReports[commitIds[i-1]], node.commitReports[commitId])
				if Debug {
					fmt.Printf("Delta resources: len %v\n", len(node.commitDeltaResources[commitId]))
				}
			}
		}
	}

	for i := 0; i < len(commitIds); i++ {
		log.Printf("Grouping: %s", commitIds[i].String())
		for _, node := range nodes {
			for _, dr := range node.commitDeltaResources[commitIds[i]] {
				groupResources(commitIds[i], dr, nodes, groupedCommits)
			}
		}
	}

	var c *git.Commit
	var gc []*groupResource
	for i := 1; i < len(commitIds); i++ {
		c, err = repo.LookupCommit(&commitIds[i])
		if err != nil {
			log.Fatal("Could not lookup commit:", err)
		}
		fmt.Printf("## Puppet noop output for commit: '%v'\n\n", c.Summary())
		fmt.Printf("%s", commitToMarkdown(c))
		gc = groupedCommits[commitIds[i]]
		fmt.Printf("%s", groupResourcesToMarkdown(gc))
	}

	// GitHub
	// githubCreate("v16", commits, groupedCommits)

	// cleanup
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal("could not create memory profile: ", err)
		}
		defer f.Close()
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
	}
}