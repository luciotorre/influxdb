package tsdb

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Point defines the values that will be written to the database
type Point interface {
	Name() string
	SetName(string)

	Tags() Tags
	AddTag(key, value string)
	SetTags(tags Tags)

	Fields() Fields
	AddField(name string, value interface{})

	Time() time.Time
	SetTime(t time.Time)
	UnixNano() int64

	HashID() uint64
	Key() []byte

	Data() []byte
	SetData(buf []byte)

	String() string
}

// point is the default implementation of Point.
type point struct {
	time time.Time

	// text encoding of measurement and tags
	// key must always be stored sorted by tags, if the original line was not sorted,
	// we need to resort it
	key []byte

	// text encoding of field data
	fields []byte

	// text encoding of timestamp
	ts []byte

	// binary encoded field data
	data []byte
}

var escapeCodes = map[byte][]byte{
	',': []byte(`\,`),
	'"': []byte(`\"`),
	' ': []byte(`\ `),
	'=': []byte(`\=`),
}

var escapeCodesStr = map[string]string{}

func init() {
	for k, v := range escapeCodes {
		escapeCodesStr[string(k)] = string(v)
	}
}

func ParsePointsString(buf string) ([]Point, error) {
	return ParsePoints([]byte(buf))
}

// ParsePoints returns a slice of Points from a text representation of a point
// with each point separated by newlines.
func ParsePoints(buf []byte) ([]Point, error) {
	return ParsePointsWithPrecision(buf, time.Now().UTC(), "n")
}

func ParsePointsWithPrecision(buf []byte, defaultTime time.Time, precision string) ([]Point, error) {
	points := []Point{}
	var (
		pos   int
		block []byte
	)
	for {
		pos, block = scanTo(buf, pos, '\n')
		pos += 1

		if len(block) == 0 {
			break
		}

		pt, err := parsePoint(block, defaultTime, precision)
		if err != nil {
			return nil, fmt.Errorf("unable to parse '%s': %v", string(block), err)
		}
		points = append(points, pt)

		if pos >= len(buf) {
			break
		}

	}
	return points, nil

}

func parsePoint(buf []byte, defaultTime time.Time, precision string) (Point, error) {
	// scan the first block which is measurement[,tag1=value1,tag2=value=2...]
	pos, key, err := scanKey(buf, 0)
	if err != nil {
		return nil, err
	}

	// measurement name is required
	if len(key) == 0 {
		return nil, fmt.Errorf("missing measurement")
	}

	// scan the second block is which is field1=value1[,field2=value2,...]
	pos, fields, err := scanFields(buf, pos)
	if err != nil {
		return nil, err
	}

	// at least one field is required
	if len(fields) == 0 {
		return nil, fmt.Errorf("missing fields")
	}

	// scan the last block which is an optional integer timestamp
	pos, ts, err := scanTime(buf, pos)

	if err != nil {
		return nil, err
	}

	pt := &point{
		key:    key,
		fields: fields,
		ts:     ts,
	}

	if len(ts) == 0 {
		pt.time = defaultTime
		pt.SetPrecision(precision)
	} else {
		ts, err := strconv.ParseInt(string(ts), 10, 64)
		if err != nil {
			return nil, err
		}
		pt.time = time.Unix(0, ts*pt.GetPrecisionMultiplier(precision))
	}
	return pt, nil
}

// scanKey scans buf starting at i for the measurement and tag portion of the point.
// It returns the ending position and the byte slice of key within buf.  If there
// are tags, they will be sorted if they are not already.
func scanKey(buf []byte, i int) (int, []byte, error) {
	start := skipWhitespace(buf, i)

	i = start

	// Determiens whether the tags are sort, assume they are
	sorted := true

	// indices holds the indexes within buf of the start of each tag.  For example,
	// a buf of 'cpu,host=a,region=b,zone=c' would have indices slice of [4,11,20]
	// which indicates that the first tag starts at buf[4], seconds at buf[11], and
	// last at buf[20]
	indices := make([]int, 100)

	// tracks how many commas we've seen so we know how many values are indices.
	// Since indices is an arbitraily large slice,
	// we need to know how many values in the buffer are in use.
	separators := 0

	// tracks whether we've see an '='
	hasSeparator := false

	// loop over each byte in buf
	for {
		// reached the end of buf?
		if i >= len(buf) {
			if !hasSeparator && separators > 0 {
				return i, buf[start:i], fmt.Errorf("missing value")
			}

			break
		}

		if buf[i] == '=' {
			i += 1
			hasSeparator = true
			continue
		}

		// escaped character
		if buf[i] == '\\' {
			i += 2
			continue
		}

		// At a tag separator (comma), track it's location
		if buf[i] == ',' {
			if !hasSeparator && separators > 0 {
				return i, buf[start:i], fmt.Errorf("missing value")
			}
			i += 1
			indices[separators] = i
			separators += 1
			hasSeparator = false
			continue
		}

		// reached end of the block? (next block would be fields)
		if buf[i] == ' ' {
			if !hasSeparator && separators > 0 {
				return i, buf[start:i], fmt.Errorf("missing value")
			}
			indices[separators] = i + 1
			break
		}

		i += 1
	}

	// Now we know where the key region is within buf, and the locations of tags, we
	// need to deterimine if duplicate tags exist and if the tags are sorted.  This iterates
	// 1/2 of the list comparing each end with each other, walking towards the center from
	// both sides.
	for j := 0; j < separators/2; j++ {
		// get the left and right tags
		_, left := scanTo(buf[indices[j]:indices[j+1]-1], 0, '=')
		_, right := scanTo(buf[indices[separators-j-1]:indices[separators-j]-1], 0, '=')

		// If the tags are equal, then there are duplicate tags, and we should abort
		if bytes.Equal(left, right) {
			return i, buf[start:i], fmt.Errorf("duplicate tags")
		}

		// If left is greater than right, the tags are not sorted.  We must continue
		// since their could be duplicate tags still.
		if bytes.Compare(left, right) > 0 {
			sorted = false
		}
	}

	// If the tags are not sorted, then sort them.  This sort is inline and
	// uses the tag indices we created earlier.  The actual buffer is not sorted, the
	// indices are using the buffer for value comparison.  After the indices are sorted,
	// the buffer is reconstructed from the sorted indices.
	if !sorted && separators > 0 {
		// Get the measurement name for later
		measurement := buf[start : indices[0]-1]

		// Sort the indices
		indices := indices[:separators]
		insertionSort(0, separators, buf, indices)

		// Create a new key using the measurement and sorted indices
		b := make([]byte, len(buf[start:i]))
		pos := copy(b, measurement)
		for _, i := range indices {
			b[pos] = ','
			pos += 1
			_, v := scanToSpaceOr(buf, i, ',')
			pos += copy(b[pos:], v)
		}

		return i, b, nil
	}

	return i, buf[start:i], nil
}

func insertionSort(l, r int, buf []byte, indices []int) {
	for i := l + 1; i < r; i++ {
		for j := i; j > l && less(buf, indices, j, j-1); j-- {
			indices[j], indices[j-1] = indices[j-1], indices[j]
		}
	}
}

func less(buf []byte, indices []int, i, j int) bool {
	// This grabs the tag names for i & j, it ignores the values
	_, a := scanTo(buf, indices[i], '=')
	_, b := scanTo(buf, indices[j], '=')
	return bytes.Compare(a, b) < 0
}

// scanFields scans buf, starting at i for the fields section of a point.  It returns
// the ending position and the byte slice of the fields within buf
func scanFields(buf []byte, i int) (int, []byte, error) {
	start := skipWhitespace(buf, i)
	i = start
	quoted := false
	for {
		// reached the end of buf?
		if i >= len(buf) {
			break
		}

		// escaped character
		if buf[i] == '\\' {
			i += 2
			continue
		}

		// If the value is quoted, scan until we get to the end quote
		if buf[i] == '"' {
			quoted = !quoted
			i += 1
			continue
		}

		// If we see an =, ensure that there is at least on char after it
		if buf[i] == '=' && !quoted {
			// check for "... value="
			if i+1 >= len(buf) {
				return i, buf[start:i], fmt.Errorf("missing field value")
			}

			// check for "... value=,value2=..."
			if buf[i+1] == ',' || buf[i+1] == ' ' {
				return i, buf[start:i], fmt.Errorf("missing field value")
			}

			if isNumeric(buf[i+1]) || buf[i+1] == '-' {
				var err error
				i, _, err = scanNumber(buf, i+1)
				if err != nil {
					return i, buf[start:i], err
				} else {
					continue
				}
				// If next byte is not a double-quote, the value must be a boolean
			} else if buf[i+1] != '"' {
				var err error
				i, _, err = scanBoolean(buf, i+1)
				if err != nil {
					return i, buf[start:i], err
				} else {
					continue
				}
			}
		}

		// reached end of block?
		if buf[i] == ' ' && !quoted {
			break
		}
		i += 1
	}
	if quoted {
		return i, buf[start:i], fmt.Errorf("unbalanced quotes")
	}
	return i, buf[start:i], nil
}

// scanTime scans buf, starting at i for the time section of a point.  It returns
// the ending position and the byte slice of the fields within buf and error if the
// timestamp is not in the correct numeric format
func scanTime(buf []byte, i int) (int, []byte, error) {
	start := skipWhitespace(buf, i)
	i = start
	for {
		// reached the end of buf?
		if i >= len(buf) {
			break
		}

		// Timestamps should integers, make sure they are so we don't need to actually
		// parse the timestamp until needed
		if buf[i] < '0' || buf[i] > '9' {
			return i, buf[start:i], fmt.Errorf("bad timestamp")
		}

		// reached end of block?
		if buf[i] == '\n' {
			break
		}
		i += 1
	}
	return i, buf[start:i], nil
}

func isNumeric(b byte) bool {
	return (b >= '0' && b <= '9') || b == '.'
}

// scanNumber returns the end position within buf, start at i after
// scanning over buf for an integer, or float.  It returns an
// error if a invalid number is scanned.
func scanNumber(buf []byte, i int) (int, []byte, error) {
	start := i

	// Is negative number?
	if i < len(buf) && buf[i] == '-' {
		i += 1
	}

	decimals := 0

	for {
		if i >= len(buf) {
			break
		}

		if buf[i] == ',' || buf[i] == ' ' {
			break
		}

		if buf[i] == '.' {
			decimals += 1
		}

		// Can't have more than 1 decimal (e.g. 1.1.1 should fail)
		if decimals > 1 {
			return i, buf[start:i], fmt.Errorf("invalid number")
		}

		// `e` is valid for floats but not as the first char
		if i > start && (buf[i] == 'e') {
			i += 1
			continue
		}

		// + and - are only valid at this point if they follow an e (scientific notation)
		if (buf[i] == '+' || buf[i] == '-') && buf[i-1] == 'e' {
			i += 1
			continue
		}

		if !isNumeric(buf[i]) {
			return i, buf[start:i], fmt.Errorf("invalid number")
		}
		i += 1
	}

	return i, buf[start:i], nil
}

// scanBoolean returns the end position within buf, start at i after
// scanning over buf for boolean. Valid values for a boolean are
// t, T, true, TRUE, f, F, false, FALSE.  It returns an error if a invalid boolean
// is scanned.
func scanBoolean(buf []byte, i int) (int, []byte, error) {
	start := i

	if i < len(buf) && (buf[i] != 't' && buf[i] != 'f' && buf[i] != 'T' && buf[i] != 'F') {
		return i, buf[start:i], fmt.Errorf("invalid boolean")
	}

	i += 1
	for {
		if i >= len(buf) {
			break
		}

		if buf[i] == ',' || buf[i] == ' ' {
			break
		}
		i += 1
	}

	// Single char bool (t, T, f, F) is ok
	if i-start == 1 {
		return i, buf[start:i], nil
	}

	// length must be 4 for true or TRUE
	if (buf[start] == 't' || buf[start] == 'T') && i-start != 4 {
		return i, buf[start:i], fmt.Errorf("invalid boolean")
	}

	// length must be 5 for false or FALSE
	if (buf[start] == 'f' || buf[start] == 'F') && i-start != 5 {
		return i, buf[start:i], fmt.Errorf("invalid boolean")
	}

	// Otherwise
	valid := false
	switch buf[start] {
	case 't':
		valid = bytes.Equal(buf[start:i], []byte("true"))
	case 'f':
		valid = bytes.Equal(buf[start:i], []byte("false"))
	case 'T':
		valid = bytes.Equal(buf[start:i], []byte("TRUE"))
	case 'F':
		valid = bytes.Equal(buf[start:i], []byte("FALSE"))
	}

	if !valid {
		return i, buf[start:i], fmt.Errorf("invalid boolean")
	}

	return i, buf[start:i], nil

}

// skipWhitespace returns the end position within buf, starting at i after
// scanning over spaces in tags
func skipWhitespace(buf []byte, i int) int {
	for {
		if i >= len(buf) {
			return i
		}

		if buf[i] == ' ' || buf[i] == '\t' {
			i += 1
			continue
		}
		break
	}
	return i
}

// scanTo returns the end position in buf and the next consecutive block
// of bytes, starting from i and ending with stop byte.  If there are leading
// spaces, they are skipped.
func scanTo(buf []byte, i int, stop byte) (int, []byte) {
	start := i
	for {
		// reached the end of buf?
		if i >= len(buf) {
			break
		}

		// reached end of block?
		if buf[i] == stop {
			break
		}
		i += 1
	}

	return i, buf[start:i]
}

// scanTo returns the end position in buf and the next consecutive block
// of bytes, starting from i and ending with stop byte.  If there are leading
// spaces, they are skipped.
func scanToSpaceOr(buf []byte, i int, stop byte) (int, []byte) {
	start := i
	for {
		// reached the end of buf?
		if i >= len(buf) {
			break
		}

		// reached end of block?
		if buf[i] == stop || buf[i] == ' ' {
			break
		}
		i += 1
	}

	return i, buf[start:i]
}

func scanTagValue(buf []byte, i int) (int, []byte) {
	start := i
	for {
		if i >= len(buf) {
			break
		}

		if buf[i] == '\\' {
			i += 2
			continue
		}

		if buf[i] == ',' {
			break
		}
		i += 1
	}
	return i, buf[start:i]
}

func scanFieldValue(buf []byte, i int) (int, []byte) {
	start := i
	quoted := false
	for {
		if i >= len(buf) {
			break
		}

		if buf[i] == '"' {
			i += 1
			quoted = !quoted
			continue
		}

		if buf[i] == '\\' {
			i += 2
			continue
		}

		if buf[i] == ',' && !quoted {
			break
		}
		i += 1
	}
	return i, buf[start:i]
}

func escape(in []byte) []byte {
	for b, esc := range escapeCodes {
		in = bytes.Replace(in, []byte{b}, esc, -1)
	}
	return in
}

func escapeString(in string) string {
	for b, esc := range escapeCodesStr {
		in = strings.Replace(in, b, esc, -1)
	}
	return in
}

func unescape(in []byte) []byte {
	for b, esc := range escapeCodes {
		in = bytes.Replace(in, esc, []byte{b}, -1)
	}
	return in
}

func unescapeString(in string) string {
	for b, esc := range escapeCodesStr {
		in = strings.Replace(in, esc, b, -1)
	}
	return in
}

// NewPoint returns a new point with the given measurement name, tags, fiels and timestamp
func NewPoint(name string, tags Tags, fields Fields, time time.Time) Point {
	return &point{
		key:    makeKey([]byte(name), tags),
		time:   time,
		fields: fields.MarshalBinary(),
	}
}

func (p *point) Data() []byte {
	return p.data
}

func (p *point) SetData(b []byte) {
	p.data = b
}

func (p *point) Key() []byte {
	return p.key
}

func (p *point) name() []byte {
	_, name := scanTo(p.key, 0, ',')
	return name
}

// Name return the measurement name for the point
func (p *point) Name() string {
	return string(unescape(p.name()))
}

// SetName updates the measurement name for the point
func (p *point) SetName(name string) {
	p.key = makeKey([]byte(name), p.Tags())
}

// Time return the timesteamp for the point
func (p *point) Time() time.Time {
	return p.time
}

// SetTime updates the timestamp for the point
func (p *point) SetTime(t time.Time) {
	p.time = t
}

// Tags returns the tag set for the point
func (p *point) Tags() Tags {
	tags := map[string]string{}

	if len(p.key) != 0 {
		pos, name := scanTo(p.key, 0, ',')

		// it's an empyt key, so there are no tags
		if len(name) == 0 {
			return tags
		}

		i := pos + 1
		var key, value []byte
		for {
			if i >= len(p.key) {
				break
			}
			i, key = scanTo(p.key, i, '=')
			i, value = scanTagValue(p.key, i+1)

			tags[string(unescape(key))] = string(unescape(value))

			i += 1
		}
	}
	return tags
}

func makeKey(name []byte, tags Tags) []byte {
	return append(escape(name), tags.hashKey()...)
}

// SetTags replaces the tags for the point
func (p *point) SetTags(tags Tags) {
	p.key = makeKey(p.name(), tags)
}

// AddTag adds or replaces a tag value for a point
func (p *point) AddTag(key, value string) {
	tags := p.Tags()
	tags[key] = value
	p.key = makeKey(p.name(), tags)
}

// Fields returns the fiels for the point
func (p *point) Fields() Fields {
	return p.unmarshalBinary()
}

// AddField adds or replaces a field value for a point
func (p *point) AddField(name string, value interface{}) {
	fields := p.Fields()
	fields[name] = value
	p.fields = fields.MarshalBinary()
}

// SetPrecision will round a time to the specified precision
func (p *point) SetPrecision(precision string) {
	switch precision {
	case "n":
	case "u":
		p.SetTime(p.Time().Truncate(time.Microsecond))
	case "ms":
		p.SetTime(p.Time().Truncate(time.Millisecond))
	case "s":
		p.SetTime(p.Time().Truncate(time.Second))
	case "m":
		p.SetTime(p.Time().Truncate(time.Minute))
	case "h":
		p.SetTime(p.Time().Truncate(time.Hour))
	}
}

// GetPrecisionMultiplier will return a multiplier for the precision specified
func (p *point) GetPrecisionMultiplier(precision string) int64 {
	d := time.Nanosecond
	switch precision {
	case "u":
		d = time.Microsecond
	case "ms":
		d = time.Millisecond
	case "s":
		d = time.Second
	case "m":
		d = time.Minute
	case "h":
		d = time.Hour
	}
	return int64(d)
}

func (p *point) String() string {
	if p.Time().IsZero() {
		return fmt.Sprintf("%s %s", p.Key(), string(p.fields))
	}
	return fmt.Sprintf("%s %s %d", p.Key(), string(p.fields), p.UnixNano())
}

func (p *point) unmarshalBinary() Fields {
	return newFieldsFromBinary(p.fields)
}

func (p *point) HashID() uint64 {
	h := fnv.New64a()
	h.Write(p.key)
	sum := h.Sum64()
	return sum
}

func (p *point) UnixNano() int64 {
	return p.Time().UnixNano()
}

type Tags map[string]string

func (t Tags) hashKey() []byte {
	// Empty maps marshal to empty bytes.
	if len(t) == 0 {
		return nil
	}

	escaped := Tags{}
	for k, v := range t {
		ek := escapeString(k)
		ev := escapeString(v)
		escaped[ek] = ev
	}

	// Extract keys and determine final size.
	sz := len(escaped) + (len(escaped) * 2) // separators
	keys := make([]string, len(escaped)+1)
	i := 0
	for k, v := range escaped {
		keys[i] = k
		i += 1
		sz += len(k) + len(v)
	}
	keys = keys[:i]
	sort.Strings(keys)
	// Generate marshaled bytes.
	b := make([]byte, sz)
	buf := b
	idx := 0
	for _, k := range keys {
		buf[idx] = ','
		idx += 1
		copy(buf[idx:idx+len(k)], k)
		idx += len(k)
		buf[idx] = '='
		idx += 1
		v := escaped[k]
		copy(buf[idx:idx+len(v)], v)
		idx += len(v)
	}
	return b[:idx]
}

type Fields map[string]interface{}

func parseNumber(val []byte) (interface{}, error) {
	for i := 0; i < len(val); i++ {
		if val[i] == '.' {
			return strconv.ParseFloat(string(val), 64)
		}
		if val[i] < '0' && val[i] > '9' {
			return string(val), nil
		}
	}
	return strconv.ParseInt(string(val), 10, 64)
}

func newFieldsFromBinary(buf []byte) Fields {
	fields := Fields{}
	var (
		i              int
		name, valueBuf []byte
		value          interface{}
		err            error
	)
	for {
		if i >= len(buf) {
			break
		}

		i, name = scanTo(buf, i, '=')
		if len(name) == 0 {
			continue
		}

		i, valueBuf = scanFieldValue(buf, i+1)
		if len(valueBuf) == 0 {
			fields[string(name)] = nil
			continue
		}

		// If the first char is a double-quote, then unmarshal as string
		if valueBuf[0] == '"' {
			value = unescapeString(string(valueBuf[1 : len(valueBuf)-1]))
			// Check for numeric characters
		} else if (valueBuf[0] >= '0' && valueBuf[0] <= '9') || valueBuf[0] == '-' || valueBuf[0] == '.' {
			value, err = parseNumber(valueBuf)
			if err != nil {
				panic(fmt.Sprintf("unable to parse number value '%v': %v", string(valueBuf), err))
			}

			// Otherwise parse it as bool
		} else {
			value, err = strconv.ParseBool(string(valueBuf))
			if err != nil {
				panic(fmt.Sprintf("unable to parse bool value '%v': %v\n", string(valueBuf), err))
			}
		}
		fields[string(unescape(name))] = value
		i += 1
	}
	return fields
}

func (p Fields) MarshalBinary() []byte {
	b := []byte{}
	keys := make([]string, len(p))
	i := 0
	for k, _ := range p {
		keys[i] = k
		i += 1
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := p[k]
		b = append(b, []byte(escapeString(k))...)
		b = append(b, '=')
		switch t := v.(type) {
		case int:
			b = append(b, []byte(strconv.FormatFloat(float64(t), 'g', -1, 64))...)
		case int32:
			b = append(b, []byte(strconv.FormatFloat(float64(t), 'g', -1, 64))...)
		case int64:
			b = append(b, []byte(strconv.FormatFloat(float64(t), 'g', -1, 64))...)
		case float64:
			// ensure there is a decimal in the encoded for

			val := []byte(strconv.FormatFloat(t, 'f', -1, 64))
			_, frac := math.Modf(t)
			hasDecimal := frac != 0
			b = append(b, val...)
			if !hasDecimal {
				b = append(b, []byte(".0")...)
			}
		case bool:
			b = append(b, []byte(strconv.FormatBool(t))...)
		case []byte:
			b = append(b, t...)
		case string:
			b = append(b, '"')
			b = append(b, []byte(t)...)
			b = append(b, '"')
		case nil:
			// skip
		default:
			panic(fmt.Sprintf("unknown type: %T", v))
		}
		b = append(b, ',')
	}
	if len(b) > 0 {
		return b[0 : len(b)-1]
	}
	return b
}

type indexedSlice struct {
	indices []int
	b       []byte
}

func (s *indexedSlice) Less(i, j int) bool {
	_, a := scanTo(s.b, s.indices[i], '=')
	_, b := scanTo(s.b, s.indices[j], '=')
	return bytes.Compare(a, b) < 0
}

func (s *indexedSlice) Swap(i, j int) {
	s.indices[i], s.indices[j] = s.indices[j], s.indices[i]
}

func (s *indexedSlice) Len() int {
	return len(s.indices)
}