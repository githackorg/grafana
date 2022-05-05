package converter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	jsoniter "github.com/json-iterator/go"
)

// helpful while debugging all the options that may appear
func logf(format string, a ...interface{}) {
	//fmt.Printf(format, a...)
}

// ReadPrometheusStyleResult will read results from a prometheus or loki server and return data frames
func ReadPrometheusStyleResult(iter *jsoniter.Iterator) *backend.DataResponse {
	var rsp *backend.DataResponse
	status := "unknown"
	errorType := ""
	err := ""
	warnings := []data.Notice{}

	for l1Field := iter.ReadObject(); l1Field != ""; l1Field = iter.ReadObject() {
		switch l1Field {
		case "status":
			status = iter.ReadString()

		case "data":
			rsp = readPrometheusData(iter)

		case "error":
			err = iter.ReadString()

		case "errorType":
			errorType = iter.ReadString()

		case "warnings":
			warnings = readWarnings(iter)

		default:
			v := iter.Read()
			logf("[ROOT] TODO, support key: %s / %v\n", l1Field, v)
		}
	}

	if status == "error" {
		return &backend.DataResponse{
			Error: fmt.Errorf("%s: %s", errorType, err),
		}
	}

	if len(warnings) > 0 {
		for _, frame := range rsp.Frames {
			if frame.Meta == nil {
				frame.Meta = &data.FrameMeta{}
			}
			frame.Meta.Notices = warnings
		}
	}

	return rsp
}

func readWarnings(iter *jsoniter.Iterator) []data.Notice {
	warnings := []data.Notice{}
	if iter.WhatIsNext() != jsoniter.ArrayValue {
		return warnings
	}

	for iter.ReadArray() {
		if iter.WhatIsNext() == jsoniter.StringValue {
			notice := data.Notice{
				Severity: data.NoticeSeverityWarning,
				Text:     iter.ReadString(),
			}
			warnings = append(warnings, notice)
		}
	}

	return warnings
}

func readPrometheusData(iter *jsoniter.Iterator) *backend.DataResponse {
	t := iter.WhatIsNext()
	if t == jsoniter.ArrayValue {
		return readArrayData(iter)
	}

	if t != jsoniter.ObjectValue {
		return &backend.DataResponse{
			Error: fmt.Errorf("expected object type"),
		}
	}

	resultType := ""
	var rsp *backend.DataResponse

	for l1Field := iter.ReadObject(); l1Field != ""; l1Field = iter.ReadObject() {
		switch l1Field {
		case "resultType":
			resultType = iter.ReadString()

		case "result":
			switch resultType {
			case "matrix":
				rsp = readMatrixOrVector(iter, resultType)
			case "vector":
				rsp = readMatrixOrVector(iter, resultType)
			case "streams":
				rsp = readStream(iter)
			case "string":
				rsp = readString(iter)
			case "scalar":
				rsp = readScalar(iter)
			default:
				iter.Skip()
				rsp = &backend.DataResponse{
					Error: fmt.Errorf("unknown result type: %s", resultType),
				}
			}

		case "stats":
			v := iter.Read()
			if len(rsp.Frames) > 0 {
				meta := rsp.Frames[0].Meta
				if meta == nil {
					meta = &data.FrameMeta{}
					rsp.Frames[0].Meta = meta
				}
				meta.Custom = map[string]interface{}{
					"stats": v,
				}
			}

		default:
			v := iter.Read()
			logf("[data] TODO, support key: %s / %v\n", l1Field, v)
		}
	}

	return rsp
}

// will return strings or exemplars
func readArrayData(iter *jsoniter.Iterator) *backend.DataResponse {
	lookup := make(map[string]*data.Field)

	var labelFrame *data.Frame
	rsp := &backend.DataResponse{}
	stringField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
	stringField.Name = "Value"
	for iter.ReadArray() {
		switch iter.WhatIsNext() {
		case jsoniter.StringValue:
			stringField.Append(iter.ReadString())

		// Either label or exemplars
		case jsoniter.ObjectValue:
			exemplar, labelPairs := readLabelsOrExemplars(iter)
			if exemplar != nil {
				rsp.Frames = append(rsp.Frames, exemplar)
			} else if labelPairs != nil {
				max := 0
				for _, pair := range labelPairs {
					k := pair[0]
					v := pair[1]
					f, ok := lookup[k]
					if !ok {
						f = data.NewFieldFromFieldType(data.FieldTypeString, 0)
						f.Name = k
						lookup[k] = f

						if labelFrame == nil {
							labelFrame = data.NewFrame("")
							rsp.Frames = append(rsp.Frames, labelFrame)
						}
						labelFrame.Fields = append(labelFrame.Fields, f)
					}
					f.Append(fmt.Sprintf("%v", v))
					if f.Len() > max {
						max = f.Len()
					}
				}

				// Make sure all fields have equal length
				for _, f := range lookup {
					diff := max - f.Len()
					if diff > 0 {
						f.Extend(diff)
					}
				}
			}

		default:
			{
				ext := iter.ReadAny()
				v := fmt.Sprintf("%v", ext)
				stringField.Append(v)
			}
		}
	}

	if rsp.Frames == nil || stringField.Len() > 0 {
		rsp.Frames = append(rsp.Frames, data.NewFrame("", stringField))
	}

	return rsp
}

// For consistent ordering read values to an array not a map
func readLabelsAsPairs(iter *jsoniter.Iterator) [][2]string {
	pairs := make([][2]string, 0, 10)
	for k := iter.ReadObject(); k != ""; k = iter.ReadObject() {
		pairs = append(pairs, [2]string{k, iter.ReadString()})
	}
	return pairs
}

func readLabelsOrExemplars(iter *jsoniter.Iterator) (*data.Frame, [][2]string) {
	pairs := make([][2]string, 0, 10)
	labels := data.Labels{}
	var frame *data.Frame

	for l1Field := iter.ReadObject(); l1Field != ""; l1Field = iter.ReadObject() {
		switch l1Field {
		case "seriesLabels":
			iter.ReadVal(&labels)
		case "exemplars":
			lookup := make(map[string]*data.Field)
			timeField := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
			valueField := data.NewFieldFromFieldType(data.FieldTypeFloat64, 0)
			valueField.Name = labels["__name__"]
			delete(labels, "__name__")
			valueField.Labels = labels
			frame = data.NewFrame("", timeField, valueField)
			frame.Meta = &data.FrameMeta{
				Custom: resultTypeToCustomMeta("exemplar"),
			}
			for iter.ReadArray() {
				for l2Field := iter.ReadObject(); l2Field != ""; l2Field = iter.ReadObject() {
					switch l2Field {
					// nolint:goconst
					case "value":
						v, _ := strconv.ParseFloat(iter.ReadString(), 64)
						valueField.Append(v)

					case "timestamp":
						ts := timeFromFloat(iter.ReadFloat64())
						timeField.Append(ts)

					case "labels":
						max := 0
						for _, pair := range readLabelsAsPairs(iter) {
							k := pair[0]
							v := pair[1]
							f, ok := lookup[k]
							if !ok {
								f = data.NewFieldFromFieldType(data.FieldTypeString, 0)
								f.Name = k
								lookup[k] = f
								frame.Fields = append(frame.Fields, f)
							}
							f.Append(v)
							if f.Len() > max {
								max = f.Len()
							}
						}

						// Make sure all fields have equal length
						for _, f := range lookup {
							diff := max - f.Len()
							if diff > 0 {
								f.Extend(diff)
							}
						}

					default:
						iter.Skip()
						frame.AppendNotices(data.Notice{
							Severity: data.NoticeSeverityError,
							Text:     fmt.Sprintf("unable to parse key: %s in response body", l2Field),
						})
					}
				}
			}
		default:
			v := fmt.Sprintf("%v", iter.Read())
			pairs = append(pairs, [2]string{l1Field, v})
		}
	}

	return frame, pairs
}

func readString(iter *jsoniter.Iterator) *backend.DataResponse {
	timeField := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
	timeField.Name = data.TimeSeriesTimeFieldName
	valueField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
	valueField.Name = data.TimeSeriesValueFieldName
	valueField.Labels = data.Labels{}

	iter.ReadArray()
	t := iter.ReadFloat64()
	iter.ReadArray()
	v := iter.ReadString()
	iter.ReadArray()

	tt := timeFromFloat(t)
	timeField.Append(tt)
	valueField.Append(v)

	frame := data.NewFrame("", timeField, valueField)
	frame.Meta = &data.FrameMeta{
		Type:   data.FrameTypeTimeSeriesMany,
		Custom: resultTypeToCustomMeta("string"),
	}

	return &backend.DataResponse{
		Frames: []*data.Frame{frame},
	}
}

func readScalar(iter *jsoniter.Iterator) *backend.DataResponse {
	timeField := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
	timeField.Name = data.TimeSeriesTimeFieldName
	valueField := data.NewFieldFromFieldType(data.FieldTypeFloat64, 0)
	valueField.Name = data.TimeSeriesValueFieldName
	valueField.Labels = data.Labels{}

	t, v, err := readTimeValuePair(iter)
	if err == nil {
		timeField.Append(t)
		valueField.Append(v)
	}

	frame := data.NewFrame("", timeField, valueField)
	frame.Meta = &data.FrameMeta{
		Type:   data.FrameTypeTimeSeriesMany,
		Custom: resultTypeToCustomMeta("scalar"),
	}

	return &backend.DataResponse{
		Frames: []*data.Frame{frame},
	}
}

func readMatrixOrVector(iter *jsoniter.Iterator, resultType string) *backend.DataResponse {
	rowIdx := 0
	timeMap := map[int64]int{}
	timeField := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
	timeField.Name = data.TimeSeriesTimeFieldName
	frame := data.NewFrame("", timeField)
	frame.Meta = &data.FrameMeta{
		Type:   data.FrameTypeTimeSeriesMany,
		Custom: resultTypeToCustomMeta(resultType),
	}
	rsp := &backend.DataResponse{
		Frames: []*data.Frame{frame},
	}

	for iter.ReadArray() {
		valueField := data.NewFieldFromFieldType(data.FieldTypeFloat64, frame.Rows())
		valueField.Name = data.TimeSeriesValueFieldName
		valueField.Labels = data.Labels{}
		frame.Fields = append(frame.Fields, valueField)

		for l1Field := iter.ReadObject(); l1Field != ""; l1Field = iter.ReadObject() {
			switch l1Field {
			case "metric":
				iter.ReadVal(&valueField.Labels)

			case "value":
				timeMap, rowIdx = addValueToFrame(frame, timeMap, rowIdx, iter)

			// nolint:goconst
			case "values":
				for iter.ReadArray() {
					timeMap, rowIdx = addValueToFrame(frame, timeMap, rowIdx, iter)
				}
			}
		}

	}

	return rsp
}

func addValueToFrame(frame *data.Frame, timeMap map[int64]int, rowIdx int, iter *jsoniter.Iterator) (map[int64]int, int) {
	timeField := frame.Fields[0]
	valueField := frame.Fields[len(frame.Fields)-1]

	t, v, err := readTimeValuePair(iter)
	if err != nil {
		return timeMap, rowIdx
	}

	ns := t.UnixNano()
	i, ok := timeMap[ns]
	if !ok {
		timeMap[ns] = rowIdx
		i = rowIdx
		expandFrame(frame, i)
		rowIdx++
	}

	timeField.Set(i, t)
	valueField.Set(i, v)

	return timeMap, rowIdx
}

func expandFrame(frame *data.Frame, idx int) {
	for _, f := range frame.Fields {
		if idx+1 > f.Len() {
			f.Extend(idx + 1 - f.Len())
		}
	}
}

func readTimeValuePair(iter *jsoniter.Iterator) (time.Time, float64, error) {
	iter.ReadArray()
	t := iter.ReadFloat64()
	iter.ReadArray()
	v := iter.ReadString()
	iter.ReadArray()

	tt := timeFromFloat(t)
	fv, err := strconv.ParseFloat(v, 64)
	return tt, fv, err
}

func readStream(iter *jsoniter.Iterator) *backend.DataResponse {
	rsp := &backend.DataResponse{}

	labelsField := data.NewFieldFromFieldType(data.FieldTypeJSON, 0)
	labelsField.Name = "__labels" // avoid automatically spreading this by labels

	timeField := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
	timeField.Name = "Time"

	lineField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
	lineField.Name = "Line"

	// Nanoseconds time field
	tsField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
	tsField.Name = "TS"

	labels := data.Labels{}
	labelJson, err := labelsToRawJson(labels)
	if err != nil {
		return &backend.DataResponse{Error: err}
	}

	for iter.ReadArray() {
		for l1Field := iter.ReadObject(); l1Field != ""; l1Field = iter.ReadObject() {
			switch l1Field {
			case "stream":
				iter.ReadVal(&labels)
				labelJson, err = labelsToRawJson(labels)
				if err != nil {
					return &backend.DataResponse{Error: err}
				}

			case "values":
				for iter.ReadArray() {
					iter.ReadArray()
					ts := iter.ReadString()
					iter.ReadArray()
					line := iter.ReadString()
					iter.ReadArray()

					t := timeFromLokiString(ts)

					labelsField.Append(labelJson)
					timeField.Append(t)
					lineField.Append(line)
					tsField.Append(ts)
				}
			}
		}
	}

	frame := data.NewFrame("", labelsField, timeField, lineField, tsField)
	frame.Meta = &data.FrameMeta{}
	rsp.Frames = append(rsp.Frames, frame)

	return rsp
}

func resultTypeToCustomMeta(resultType string) map[string]string {
	return map[string]string{"resultType": resultType}
}

func timeFromFloat(fv float64) time.Time {
	return time.UnixMilli(int64(fv * 1000.0)).UTC()
}

func timeFromLokiString(str string) time.Time {
	// normal time values look like: 1645030246277587968
	// and are less than: math.MaxInt65=9223372036854775807
	// This will do a fast path for any date before 2033
	s := len(str)
	if s < 19 || (s == 19 && str[0] == '1') {
		ns, err := strconv.ParseInt(str, 10, 64)
		if err == nil {
			return time.Unix(0, ns).UTC()
		}
	}

	ss, _ := strconv.ParseInt(str[0:10], 10, 64)
	ns, _ := strconv.ParseInt(str[10:], 10, 64)
	return time.Unix(ss, ns).UTC()
}

func labelsToRawJson(labels data.Labels) (json.RawMessage, error) {
	// data.Labels when converted to JSON keep the fields sorted
	bytes, err := jsoniter.Marshal(labels)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(bytes), nil
}
