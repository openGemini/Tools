/*
Copyright (c) 2013-2018 InfluxData Inc.
this code is originally from https://github.com/influxdata/influxdb/tree/v1.8.2/tsdb/engine/tsm1/file_store.go
the implementations of the "location" and "KeyCursor" classes in the above file are referenced

2023.05.11 Changed
* simplifies the structure of location and keycursor, preserving only the necessary parts
* changed the way the block data pointed to by location is read to make it easier to implement
* implements a block reading function that uses the tsm1.Values interface instead of using the
  block reader for the corresponding data type to read blocks
* implemented a scanner to simply simulate the way influxdb's upper layer iterator assembling data
copyright 2023 Qizhi Huang(flaggyellow@qq.com)
*/

package main

import (
    "fmt"
    "math"
    "sort"
    "time"

    "github.com/influxdata/influxdb/tsdb/engine/tsm1"
    client "github.com/influxdata/influxdb1-client/v2"
)

type location struct {
    r     tsm1.TSMFile
    entry tsm1.IndexEntry

    readMax int64
}

type ascLocations []*location

// Sort methods
func (a ascLocations) Len() int      { return len(a) }
func (a ascLocations) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ascLocations) Less(i, j int) bool {
    return a[i].entry.MinTime < a[j].entry.MinTime
}

type Cursor struct {
    et     int64
    readTs int64
    key    []byte
    // seeks is all the file locations that we need to return during iteration.
    seeks []*location
    buf   []tsm1.Value
    pos   int
}

func (c *Cursor) init() error {
    if len(c.seeks) == 0 {
        c.buf = nil
        c.pos = 0
        return nil
    }
    sort.Sort(ascLocations(c.seeks))
    for _, e := range c.seeks {
        if e.readMax < e.entry.MinTime-1 {
            e.readMax = e.entry.MinTime - 1
        }
    }
    c.readTs = c.seeks[0].readMax
    if logger.IsDebug() {
        // check the validation of readMax
        for i := 1; i < len(c.seeks); i++ {
            if c.seeks[i].readMax < c.seeks[i-1].readMax {
                logger.LogString("Cursor.init: found readMax not in right order", TOLOGFILE, LEVEL_DEBUG)
            }
        }
    }
    var err error
    c.buf, err = c.readBlock()
    if err != nil {
        logger.LogString("Read block failed: "+err.Error(), TOLOGFILE, LEVEL_ERROR)
    }
    c.pos = 0
    return err
}

func (c *Cursor) readBlock() (tsm1.Values, error) {
    // No matching blocks to decode
    if len(c.seeks) == 0 || c.readTs >= c.et {
        return nil, nil
    }

    // check the validation of readTs
    if logger.IsDebug() {
        if c.readTs > c.seeks[0].readMax {
            logger.LogString("Cursor.readBlock: readTs > c.seeks[0].readMax", TOLOGFILE, LEVEL_DEBUG)
        }
    }

    var locsToRead []*location
    locsToRead = append(locsToRead, c.seeks[0])

    // determine the locations to read this time
    for i := 1; i < len(c.seeks); i++ {
        if c.seeks[i].readMax == c.readTs {
            locsToRead = append(locsToRead, c.seeks[i])
        }
    }

    // determine the max timestamp to read this time
    // the timestamp range to read is [c.readTs, upperBound]
    upperBound := c.et
    for _, e := range locsToRead {
        if e.entry.MaxTime < upperBound {
            upperBound = e.entry.MaxTime
        }
    }
    if len(c.seeks) > len(locsToRead) {
        nextRoundStartTs := c.seeks[len(locsToRead)].readMax
        if nextRoundStartTs <= upperBound {
            upperBound = nextRoundStartTs
        }
    }

    // this should not happen
    if upperBound <= c.readTs {
        logger.LogString("Cursor.readBlock: found upperBound <= readTs", TOLOGFILE, LEVEL_ERROR)
        // resolve the problem
        sort.Slice(c.seeks, func(i, j int) bool {
            return c.seeks[i].readMax < c.seeks[j].readMax
        })
        for i := 0; i < len(c.seeks); i++ {
            rm := c.seeks[i].readMax
            if rm < c.readTs {
                logger.LogString("Cursor.readBlock: found readMax < readTs", TOLOGFILE, LEVEL_DEBUG)
                c.seeks[i].readMax = c.readTs
                rm = c.readTs
            }
            if rm >= c.et || rm >= c.seeks[i].entry.MaxTime {
                c.seeks = append(c.seeks[:i], c.seeks[i+1:]...)
                i--
            }
        }
        return c.readBlock()
    }

    var buf []tsm1.Value

    for _, e := range locsToRead {
        tombstones := e.r.TombstoneRange(c.key)
        values, err := e.r.(*tsm1.TSMReader).ReadAt(&e.entry, nil)
        if err != nil {
            logger.LogString("Read block failed: "+err.Error(), TOLOGFILE, LEVEL_ERROR)
            return nil, err
        }
        for _, v := range values {
            ts := v.UnixNano()
            if ts <= c.readTs {
                continue
            }
            if ts > upperBound {
                break
            }
            for _, tomb := range tombstones {
                if ts > tomb.Min && ts < tomb.Max {
                    continue
                }
            }
            buf = append(buf, v)
        }
        e.readMax = upperBound
    }

    // drop the locations that finish read
    for i := 0; i < len(c.seeks); i++ {
        rm := c.seeks[i].readMax
        if rm >= c.et || rm >= c.seeks[i].entry.MaxTime {
            c.seeks = append(c.seeks[:i], c.seeks[i+1:]...)
            i--
        }
    }

    // mark the time range that have been read
    tmpReadTs := c.readTs
    c.readTs = upperBound

    if len(buf) <= 0 {
        logger.LogString(fmt.Sprintf("Cursor.readBlock: the buffer is empty with %d locations reading, readTs %d, upperbound %d",
            len(locsToRead), tmpReadTs, upperBound), TOLOGFILE, LEVEL_DEBUG)
        return c.readBlock()
    }

    return sortAndDeduplicateValues(&buf), nil
}

func (c *Cursor) peek() (tsm1.Value, error) {
    if len(c.buf) > 0 && c.pos < len(c.buf) {
        return c.buf[c.pos], nil
    }
    var err error
    c.buf, err = c.readBlock()
    if err != nil {
        return nil, err
    }
    c.pos = 0
    if c.buf == nil {
        return nil, nil
    }
    return c.peek()
}

func (c *Cursor) next() (tsm1.Value, error) {
    if len(c.buf) > 0 && c.pos < len(c.buf) {
        c.pos++
        return c.buf[c.pos-1], nil
    }
    var err error
    c.buf, err = c.readBlock()
    if err != nil {
        return nil, err
    }
    c.pos = 0
    if c.buf == nil {
        return nil, nil
    }
    return c.next()
}

// referenced from https://github.com/influxdata/influxdb/tree/v1.8.2/tsdb/engine/tsm1/encoding.gen.go
// function (Values).Deduplicate
func sortAndDeduplicateValues(buf *[]tsm1.Value) []tsm1.Value {
    if len(*buf) == 0 {
        logger.LogString("sortAndDeduplicateValues: empty buffer", TOLOGFILE, LEVEL_DEBUG)
        return nil
    }
    sort.Slice(*buf, func(i, j int) bool {
        return (*buf)[i].UnixNano() < (*buf)[j].UnixNano()
    })
    var i int
    for j := 1; j < len(*buf); j++ {
        v := (*buf)[j]
        if v.UnixNano() != (*buf)[i].UnixNano() {
            i++
        }
        (*buf)[i] = v
    }
    return (*buf)[:i+1]
}

type Scanner struct {
    measurement string
    tags        map[string]string
    fields      map[string]*Cursor
}

func (s *Scanner) nextPoint(cmd *DataMigrateCommand) (*client.Point, error) {
    // determine the current timestamp
    var curTs int64 = math.MaxInt64
    for _, cursor := range s.fields {
        v, err := cursor.peek()
        if err != nil {
            return nil, err
        }
        if v == nil {
            continue
        }
        if curTs > v.UnixNano() {
            curTs = v.UnixNano()
        }
    }
    if curTs == math.MaxInt64 {
        return nil, nil
    }
    // assembles points here
    fields := map[string]interface{}{}
    for field, cursor := range s.fields {
        v, err := cursor.peek()
        if err != nil {
            return nil, err
        }
        if v == nil {
            continue
        }
        if curTs == v.UnixNano() {
            _, err := cursor.next()
            if err != nil {
                return nil, err
            }
            fields[field] = v.Value()
        }
    }

    // statistics
    for t := range s.tags {
        cmd.stat.tagsRead[s.measurement+t] = struct{}{}
        cmd.stat.tagsTotal[s.measurement+t] = struct{}{}
    }
    for f := range fields {
        cmd.stat.fieldsRead[s.measurement+f] = struct{}{}
        cmd.stat.fieldTotal[s.measurement+f] = struct{}{}
    }

    return client.NewPoint(s.measurement, s.tags, fields, time.Unix(0, curTs))
}

func (s *Scanner) writeBatches(c client.Client, cmd *DataMigrateCommand) error {
    count := 0
    var bp client.BatchPoints
    flag := true
    for {
        if flag {
            bp, _ = client.NewBatchPoints(client.BatchPointsConfig{
                Database:  cmd.database,
                Precision: "ns",
            })
            flag = false
        }

        pt, err := s.nextPoint(cmd)

        if err != nil {
            logger.LogString("point read error: "+err.Error(), TOLOGFILE|TOCONSOLE, LEVEL_ERROR)
            return err
        }

        if pt == nil {
            rowsNum := len(bp.Points())
            err := c.Write(bp)
            if err != nil {
                logger.LogString("insert error: "+err.Error(), TOLOGFILE|TOCONSOLE, LEVEL_ERROR)
                return err
            }
            cmd.stat.rowsRead += rowsNum
            break
        }

        bp.AddPoint(pt)
        count = count + 1
        if count == BATCHSIZE {
            err := c.Write(bp)
            if err != nil {
                logger.LogString("insert error: "+err.Error(), TOLOGFILE|TOCONSOLE, LEVEL_ERROR)
                return err
            }
            cmd.stat.rowsRead += BATCHSIZE
            flag = true
            count = 0
        }
    }
    return nil
}
