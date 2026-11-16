//go:build wasip1

// Command audioprobe drives the host module wctr_audio_out from a real guest:
// it opens a stream, writes a tone and prints what the host answered. The
// first argument picks the case.
package main

import (
	"fmt"
	"math"
	"os"
	"unsafe"
)

//go:wasmimport wctr_audio_out stream_open
func streamOpen(rate, channels, capacity uint32, out unsafe.Pointer) uint32

//go:wasmimport wctr_audio_out stream_write
func streamWrite(stream, ptr, frames uint32, out unsafe.Pointer) uint32

//go:wasmimport wctr_audio_out stream_stats
func streamStats(stream uint32, out unsafe.Pointer) uint32

//go:wasmimport wctr_audio_out stream_close
func streamClose(stream uint32) uint32

func main() {
	var handle uint32
	e := streamOpen(11025, 2, 1024, unsafe.Pointer(&handle))
	if e != 0 {
		fmt.Println("open", e)
		return
	}
	switch os.Args[1] {
	case "tone":
		pcm := make([]float32, 2*512)
		for k := 0; k < 512; k++ {
			v := float32(0.25 * math.Sin(2*math.Pi*440*float64(k)/11025))
			pcm[2*k], pcm[2*k+1] = v, v
		}
		var res [2]uint32
		e = streamWrite(handle, uint32(uintptr(unsafe.Pointer(&pcm[0]))), 512, unsafe.Pointer(&res))
		fmt.Println("write", e, res[0], res[1])
		var st [4]uint32
		e = streamStats(handle, unsafe.Pointer(&st))
		fmt.Println("stats", e, st[3])
	case "hostile":
		// A pointer at the 32-bit edge, and one stream too many.
		var res [2]uint32
		fmt.Println("edge", streamWrite(handle, 0xFFFFFFF0, 256, unsafe.Pointer(&res)))
		fmt.Println("second", streamOpen(11025, 2, 1024, unsafe.Pointer(&handle)))
		fmt.Println("third", streamOpen(11025, 2, 1024, unsafe.Pointer(&handle)))
	}
	fmt.Println("close", streamClose(handle))
}
