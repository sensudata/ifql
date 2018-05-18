package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/influxdata/ifql/query"
	"github.com/influxdata/ifql/query/execute"
	"github.com/influxdata/ifql/query/plan"
	"github.com/influxdata/ifql/semantic"
	"github.com/influxdata/line-protocol"
	"github.com/pkg/errors"
)

const (
	ToHTTPKind           = "toHTTP"
	DefaultToHTTPTimeout = 1 * time.Second
)

// DefaultToHTTPUserAgent is the default user agent used by ToHttp
var DefaultToHTTPUserAgent = "ifqld/dev"

func newOutPutClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConnsPerHost:   runtime.GOMAXPROCS(0) + 1,
		},
	}
}

var toHTTPKeepAliveClient = newOutPutClient()

// this is used so we can get better validation on marshaling, innerToHTTPOpSpec and ToHTTPOpSpec
// need to have identical fields
type innerToHTTPOpSpec ToHTTPOpSpec

type ToHTTPOpSpec struct {
	Addr         string            `json:"addr"`
	Method       string            `json:"method"` // default behavior should be POST
	Name         string            `json:"name"`
	Headers      map[string]string `json:"headers"`   // TODO: implement Headers after bug with keys and arrays and objects is fixed (new parser implemented, with string literals as keys)
	URLParams    map[string]string `json:"urlparams"` // TODO: implement URLParams after bug with keys and arrays and objects is fixed (new parser implemented, with string literals as keys)
	Timeout      time.Duration     `json:"timeout"`   // default to something reasonable if zero
	NoKeepAlive  bool              `json:"nokeepalive"`
	TimeColumn   string            `json:"time_column"`
	TagColumns   []string          `json:"tag_columns"`
	ValueColumns []string          `json:"value_columns"`
}

func init() {
	query.RegisterFunction(ToHTTPKind, createToHTTPOpSpec, ToHTTPSignature)
	query.RegisterOpSpec(ToHTTPKind,
		func() query.OperationSpec { return &ToHTTPOpSpec{} })
	plan.RegisterProcedureSpec(ToHTTPKind, newToHTTPProcedure, ToHTTPKind)
	execute.RegisterTransformation(ToHTTPKind, createToHTTPTransformation)
}

// ReadArgs loads a query.Arguments into ToHTTPOpSpec.  It sets several default values.
// If the http method isn't set, it defaults to POST, it also uppercases the http method.
// If the time_column isn't set, it defaults to execute.TimeColLabel.
// If the value_column isn't set it defaults to a []string{execute.DefaultValueColLabel}.
func (o *ToHTTPOpSpec) ReadArgs(args query.Arguments) error {
	var err error
	o.Addr, err = args.GetRequiredString("addr")
	if err != nil {
		return err
	}

	o.Name, err = args.GetRequiredString("name")
	if err != nil {
		return err
	}

	var ok bool
	o.Method, ok, err = args.GetString("method")
	if err != nil {
		return err
	}
	if !ok {
		o.Method = "POST"
	}
	o.Method = strings.ToUpper(o.Method)

	timeout, ok, err := args.GetDuration("timeout")
	if err != nil {
		return err
	}
	if !ok {
		o.Timeout = DefaultToHTTPTimeout
	} else {
		o.Timeout = time.Duration(timeout)
	}

	o.TimeColumn, ok, err = args.GetString("time_column")
	if err != nil {
		return err
	}
	if !ok {
		o.TimeColumn = execute.DefaultTimeColLabel
	}

	tagColumns, ok, err := args.GetArray("tag_columns", semantic.String)
	if err != nil {
		return err
	}
	o.TagColumns = o.TagColumns[:0]
	if ok {
		for i := 0; i < tagColumns.Len(); i++ {
			o.TagColumns = append(o.TagColumns, tagColumns.Get(i).Str())
		}
		sort.Strings(o.TagColumns)
	}

	valueColumns, ok, err := args.GetArray("value_columns", semantic.String)
	if err != nil {
		return err
	}
	o.ValueColumns = o.ValueColumns[:0]

	if !ok || valueColumns.Len() == 0 {
		o.ValueColumns = append(o.ValueColumns, execute.DefaultValueColLabel)
	} else {
		for i := 0; i < valueColumns.Len(); i++ {
			o.TagColumns = append(o.ValueColumns, valueColumns.Get(i).Str())
		}
		sort.Strings(o.TagColumns)
	}

	// TODO: get other headers working!
	o.Headers = map[string]string{
		"Content-Type": "application/vnd.influx",
		"User-Agent":   DefaultToHTTPUserAgent,
	}

	return err

}

func createToHTTPOpSpec(args query.Arguments, a *query.Administration) (query.OperationSpec, error) {
	if err := a.AddParentFromArgs(args); err != nil {
		return nil, err
	}
	s := new(ToHTTPOpSpec)
	if err := s.ReadArgs(args); err != nil {
		return nil, err
	}
	// if err := s.AggregateConfig.ReadArgs(args); err != nil {
	// 	return s, err
	// }
	return s, nil
}

// UnmarshalJSON unmarshals and validates toHTTPOpSpec into JSON.
func (o *ToHTTPOpSpec) UnmarshalJSON(b []byte) (err error) {

	if err = json.Unmarshal(b, (*innerToHTTPOpSpec)(o)); err != nil {
		return err
	}
	u, err := url.ParseRequestURI(o.Addr)
	if err != nil {
		return err
	}
	if !(u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "") {
		return fmt.Errorf("Scheme must be http or https but was %s", u.Scheme)
	}
	return nil
}

var ToHTTPSignature = query.DefaultFunctionSignature()

func (ToHTTPOpSpec) Kind() query.OperationKind {
	return ToHTTPKind
}

type ToHTTPProcedureSpec struct {
	Spec *ToHTTPOpSpec
}

func (o *ToHTTPProcedureSpec) Kind() plan.ProcedureKind {
	return CountKind
}

func (o *ToHTTPProcedureSpec) Copy() plan.ProcedureSpec {
	return &ToHTTPProcedureSpec{}
}

func newToHTTPProcedure(qs query.OperationSpec, a plan.Administration) (plan.ProcedureSpec, error) {
	spec, ok := qs.(*ToHTTPOpSpec)
	if !ok && spec != nil {
		return nil, fmt.Errorf("invalid spec type %T", qs)
	}
	return &ToHTTPProcedureSpec{Spec: spec}, nil
}

func createToHTTPTransformation(id execute.DatasetID, mode execute.AccumulationMode, spec plan.ProcedureSpec, a execute.Administration) (execute.Transformation, execute.Dataset, error) {
	s, ok := spec.(*ToHTTPProcedureSpec)
	if !ok {
		return nil, nil, fmt.Errorf("invalid spec type %T", spec)
	}
	cache := execute.NewBlockBuilderCache(a.Allocator())
	d := execute.NewDataset(id, mode, cache)
	t := NewToHTTPTransformation(d, cache, s)
	return t, d, nil
}

type ToHTTPTransformation struct {
	d     execute.Dataset
	cache execute.BlockBuilderCache
	spec  *ToHTTPProcedureSpec
}

func (t *ToHTTPTransformation) RetractBlock(id execute.DatasetID, key execute.PartitionKey) error {
	return t.d.RetractBlock(key)
}

func NewToHTTPTransformation(d execute.Dataset, cache execute.BlockBuilderCache, spec *ToHTTPProcedureSpec) *ToHTTPTransformation {

	return &ToHTTPTransformation{
		d:     d,
		cache: cache,
		spec:  spec,
	}
}

type httpOutputMetric struct {
	tags   []*protocol.Tag
	fields []*protocol.Field
	name   string
	t      time.Time
}

func (m *httpOutputMetric) TagList() []*protocol.Tag {
	return m.tags
}
func (m *httpOutputMetric) FieldList() []*protocol.Field {
	return m.fields
}

func (m *httpOutputMetric) truncateTagsAndFields() {
	m.fields = m.fields[:0]
	m.tags = m.tags[:0]

}

func (m *httpOutputMetric) Name() string {
	return m.name
}

func (m *httpOutputMetric) Time() time.Time {
	return m.t
}

type idxType struct {
	Idx  int
	Type execute.DataType
}

func (t *ToHTTPTransformation) Process(id execute.DatasetID, b execute.Block) error {
	pr, pw := io.Pipe() // TODO: replce the pipe with something faster
	m := &httpOutputMetric{}
	e := protocol.NewEncoder(pw)
	e.FailOnFieldErr(true)
	e.SetFieldSortOrder(protocol.SortFields)
	cols := b.Cols()
	labels := make(map[string]idxType, len(cols))
	for i, col := range cols {
		labels[col.Label] = idxType{Idx: i, Type: col.Type}
	}
	// do time
	timeColLabel := t.spec.Spec.TimeColumn
	timeColIdx, ok := labels[timeColLabel]
	if !ok {
		return errors.New("Could not get time column")
	}
	if timeColIdx.Type != execute.TTime {
		return fmt.Errorf("column %s is not of type %s", timeColLabel, timeColIdx.Type)
	}
	var err error
	go func() {
		m.name = t.spec.Spec.Name
		b.Do(func(er execute.ColReader) error {
			m.truncateTagsAndFields()
			for i, col := range er.Cols() {
				switch {
				case col.Label == timeColLabel:
					m.t = er.Times(i)[0].Time()
				case sort.SearchStrings(t.spec.Spec.ValueColumns, col.Label) < len(t.spec.Spec.ValueColumns): // do thing to get values
					switch col.Type {
					case execute.TFloat:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.Floats(i)[0]})
					case execute.TInt:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.Ints(i)[0]})
					case execute.TUInt:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.UInts(i)[0]})
					case execute.TString:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.Strings(i)[0]})
					case execute.TTime:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.Times(i)[0]})
					case execute.TBool:
						m.fields = append(m.fields, &protocol.Field{Key: col.Label, Value: er.Bools(i)[0]})
					default:
						err = errors.New("invalid type")
					}
				case sort.SearchStrings(t.spec.Spec.TagColumns, col.Label) < len(t.spec.Spec.TagColumns): // do thing to get tag
					m.tags = append(m.tags, &protocol.Tag{Key: col.Label, Value: er.Strings(i)[0]})
				}
			}

			_, err := e.Encode(m)
			if err != nil {
				fmt.Println(err)
			}
			return nil
		})
		pw.Close()
	}()

	req, err := http.NewRequest(t.spec.Spec.Method, t.spec.Spec.Addr, pr)
	if err != nil {
		return err
	}

	if t.spec.Spec.Timeout <= 0 {
		ctx, cancel := context.WithTimeout(context.Background(), t.spec.Spec.Timeout)
		req = req.WithContext(ctx)
		defer cancel()
	}
	var resp *http.Response
	if t.spec.Spec.NoKeepAlive {
		resp, err = newOutPutClient().Do(req)
	} else {
		resp, err = toHTTPKeepAliveClient.Do(req)

	}
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return req.Body.Close()
}

func (t *ToHTTPTransformation) UpdateWatermark(id execute.DatasetID, pt execute.Time) error {
	return t.d.UpdateWatermark(pt)
}
func (t *ToHTTPTransformation) UpdateProcessingTime(id execute.DatasetID, pt execute.Time) error {
	return t.d.UpdateProcessingTime(pt)
}
func (t *ToHTTPTransformation) Finish(id execute.DatasetID, err error) {
	t.d.Finish(err)
}