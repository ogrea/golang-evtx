package evtx

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/0xrawsec/golang-utils/datastructs"
	"github.com/0xrawsec/golang-utils/encoding"
)

// ChunkCache structure as a Set
type ChunkCache struct {
	datastructs.SyncedSet
}

/////////////////////////////// ChunkSorter ////////////////////////////////////

// ChunkSorter structure used to sort chunks before parsing the events inside
// prevent unordered events
type ChunkSorter []Chunk

// Implement Sortable interface
func (cs ChunkSorter) Len() int {
	return len(cs)
}

// Implement Sortable interface
func (cs ChunkSorter) Less(i, j int) bool {
	return cs[i].Header.NumFirstRecLog < cs[j].Header.NumFirstRecLog
}

// Implement Sortable interface
func (cs ChunkSorter) Swap(i, j int) {
	cs[i], cs[j] = cs[j], cs[i]
}

//////////////////////////////////// File //////////////////////////////////////

// FileHeader structure definition
type FileHeader struct {
	Magic           [8]byte
	FirstChunkNum   uint64
	LastChunkNum    uint64
	NextRecordID    uint64
	HeaderSpace     uint32
	MinVersion      uint16
	MajVersion      uint16
	ChunkDataOffset uint16
	ChunkCount      uint16
	Unknown         [76]byte
	Flags           uint32
	CheckSum        uint32
}

// File structure definition
type File struct {
	Header FileHeader
	file   *os.File
}

// New EvtxFile structure initialized from file
// @filepath : filepath of the evtx file to parse
// return File : File structure initialized
func New(filepath string) (ef File, err error) {
	file, err := os.Open(filepath)
	if err != nil {
		return
	}
	ef.file = file
	ef.ParseFileHeader()
	return
}

// ParseFileHeader parses a the file header of the file structure and modifies
// the Header of the current structure
func (ef *File) ParseFileHeader() {
	GoToSeeker(ef.file, 0)
	err := encoding.Unmarshal(ef.file, &ef.Header, Endianness)
	if err != nil {
		panic(err)
	}
}

func (fh FileHeader) String() string {
	return fmt.Sprintf(
		"Magic: %s\n"+
			"FirstChunkNum: %d\n"+
			"LastChunkNum: %d\n"+
			"NumNextRecord: %d\n"+
			"HeaderSpace: %d\n"+
			"MinVersion: 0x%04x\n"+
			"MaxVersion: 0x%04x\n"+
			"SizeHeader: %d\n"+
			"ChunkCount: %d\n"+
			"Flags: 0x%08x\n"+
			"CheckSum: 0x%08x\n",
		fh.Magic,
		fh.FirstChunkNum,
		fh.LastChunkNum,
		fh.NextRecordID,
		fh.HeaderSpace,
		fh.MinVersion,
		fh.MajVersion,
		fh.ChunkDataOffset,
		fh.ChunkCount,
		fh.Flags,
		fh.CheckSum)

}

// FetchRawChunk fetches a raw Chunk (without parsing String and Template tables)
// @offset : offset in the current file where to find the Chunk
// return Chunk : Chunk (raw) parsed
func (ef *File) FetchRawChunk(offset int64) (Chunk, error) {
	c := NewChunk()
	GoToSeeker(ef.file, offset)
	c.Offset = offset
	c.Data = make([]byte, ChunkHeaderSize)
	if _, err := ef.file.Read(c.Data); err != nil {
		return c, err
	}
	reader := bytes.NewReader(c.Data)
	c.ParseChunkHeader(reader)
	return c, nil
}

// FetchChunk fetches a Chunk
// @offset : offset in the current file where to find the Chunk
// return Chunk : Chunk parsed
func (ef *File) FetchChunk(offset int64) (Chunk, error) {
	c := NewChunk()
	GoToSeeker(ef.file, offset)
	c.Offset = offset
	c.Data = make([]byte, ChunkSize)
	if _, err := ef.file.Read(c.Data); err != nil {
		return c, err
	}
	reader := bytes.NewReader(c.Data)
	c.ParseChunkHeader(reader)
	// Go to after Header
	GoToSeeker(reader, int64(c.Header.SizeHeader))
	c.ParseStringTable(reader)
	if err := c.ParseTemplateTable(reader); err != nil {
		return c, err
	}
	if err := c.ParseEventOffsets(reader); err != nil {
		return c, err
	}
	return c, nil
}

// Chunks returns a chan of all the Chunks found in the current file
// return (chan Chunk)
func (ef *File) Chunks() (cc chan Chunk) {
	cc = make(chan Chunk)
	go func() {
		defer close(cc)
		cs := make(ChunkSorter, 0, ef.Header.ChunkCount)
		for i := uint16(0); i <= ef.Header.ChunkCount; i++ {
			offsetChunk := int64(ef.Header.ChunkDataOffset) + int64(ChunkSize)*int64(i)
			chunk, err := ef.FetchRawChunk(offsetChunk)
			switch {
			case err != nil && err != io.EOF:
				panic(err)
			case err == nil:
				cs = append(cs, chunk)
			}
		}
		// We sort out the chunks
		sort.Stable(cs)
		for _, rc := range cs {
			chunk, err := ef.FetchChunk(rc.Offset)
			switch {
			case err != nil && err != io.EOF:
				panic(err)
			case err == nil:
				cc <- chunk
			}
		}
	}()
	return
}

// MonitorChunks returns a chan of all the Chunks found in the file under monitoring
// @stop: a channel used to stop the monitoring if needed
// return (chan Chunk)
func (ef *File) MonitorChunks(stop chan bool) (cc chan Chunk) {
	cc = make(chan Chunk, 4)
	sleepTime := 250 * time.Millisecond
	markedChunks := datastructs.NewSyncedSet()

	// Main routine to feed the Chunk Channel
	go func() {
		defer close(cc)
		firstLoopFlag := true
		for {
			// Parse the file header again to get the updates in the file
			ef.ParseFileHeader()

			// check if we should stop or not
			select {
			case <-stop:
				return
			default:
				// go through
			}
			curChunks := datastructs.NewSyncedSet()
			cs := make(ChunkSorter, 0, ef.Header.ChunkCount)
			for i := uint16(0); i <= ef.Header.ChunkCount; i++ {
				offsetChunk := int64(ef.Header.ChunkDataOffset) + int64(ChunkSize)*int64(i)
				chunk, err := ef.FetchRawChunk(offsetChunk)
				curChunks.Add(chunk.Header.FirstEventRecID, chunk.Header.LastEventRecID)
				// We append only the Chunks whose EventRecordIds have not been treated yet
				if markedChunks.Contains(chunk.Header.FirstEventRecID) && markedChunks.Contains(chunk.Header.LastEventRecID) {
					continue
				}
				switch {
				case err != nil && err != io.EOF:
					panic(err)
				case err == nil:
					markedChunks.Add(chunk.Header.FirstEventRecID)
					markedChunks.Add(chunk.Header.LastEventRecID)
					if !firstLoopFlag {
						cs = append(cs, chunk)
					}
				}
			}

			// Cleanup the useless cache entries (consider putting in go routine if worth)
			markedChunks = datastructs.NewSyncedSet(markedChunks.Intersect(&curChunks))

			// We flag out of first loop
			firstLoopFlag = false
			// We sort out the chunks
			sort.Stable(cs)
			for _, rc := range cs {
				chunk, err := ef.FetchChunk(rc.Offset)
				switch {
				case err != nil && err != io.EOF:
					panic(err)
				case err == nil:
					cc <- chunk
				}
			}
			// Sleep between loops
			time.Sleep(sleepTime)
		}
	}()
	return
}

// Events returns a chan pointers to all the GoEvtxMap found in the current file
// return (chan *GoEvtxMap)
func (ef *File) Events() (cgem chan *GoEvtxMap) {
	cgem = make(chan *GoEvtxMap, 1)
	go func() {
		defer close(cgem)
		for c := range ef.Chunks() {
			for e := range c.Events() {
				cgem <- e
			}
		}
	}()
	return
}

// FastEvents returns a chan pointers to all the GoEvtxMap found in the current
// file. Same as Events method but the fast version
// return (chan *GoEvtxMap)
func (ef *File) FastEvents() (cgem chan *GoEvtxMap) {
	jobs := int(math.Floor(float64(runtime.NumCPU()) / 2))
	cgem = make(chan *GoEvtxMap, 42)
	go func() {
		defer close(cgem)
		chanQueue := make(chan (chan *GoEvtxMap), jobs)
		go func() {
			defer close(chanQueue)
			for pc := range ef.Chunks() {
				// We have to create a copy here because otherwise cpc.EventsChan() fails
				// I guess that because EventsChan takes a pointer to an object and that
				// and thus the chan is taken on the pointer and since the object pointed
				// changes -> kaboom
				cpc := pc
				ev := cpc.Events()
				chanQueue <- ev
			}
		}()
		for ec := range chanQueue {
			for event := range ec {
				cgem <- event
			}
		}
	}()
	return
}

// MonitorEvents returns a chan pointers to all the GoEvtxMap found in the File
// under monitoring. This is the fast version
// @stop: a channel used to stop the monitoring if needed
// return (chan *GoEvtxMap)
func (ef *File) MonitorEvents(stop chan bool) (cgem chan *GoEvtxMap) {
	// Normally, it should not be needed to add a second check here on the
	// EventRecordID since the record ids in the chunks are not supposed to overlap
	// TODO: Add a EventRecordID marker if needed
	jobs := int(math.Floor(float64(runtime.NumCPU()) / 2))
	cgem = make(chan *GoEvtxMap, 42)
	go func() {
		defer close(cgem)
		chanQueue := make(chan (chan *GoEvtxMap), jobs)
		go func() {
			defer close(chanQueue)
			// this chan ends only when value is put into stop
			for pc := range ef.MonitorChunks(stop) {
				// We have to create a copy here because otherwise cpc.EventsChan() fails
				// I guess that because EventsChan takes a pointer to an object
				// and thus the chan is taken on the pointer and since the object pointed
				// changes -> kaboom
				cpc := pc
				ev := cpc.Events()
				chanQueue <- ev
			}
		}()
		for ec := range chanQueue {
			for event := range ec {
				cgem <- event
			}
		}
	}()
	return
}