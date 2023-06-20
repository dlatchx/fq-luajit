package luajit

// dump   = header proto+ 0U
// header = ESC 'L' 'J' versionB flagsU [namelenU nameB*]
// proto  = lengthU pdata
// pdata  = phead bcinsW* uvdataH* kgc* knum* [debugB*]
// phead  = flagsB numparamsB framesizeB numuvB numkgcU numknU numbcU
//          [debuglenU [firstlineU numlineU]]
// kgc    = kgctypeU { ktab | (loU hiU) | (rloU rhiU iloU ihiU) | strB* }
// knum   = intU0 | (loU1 hiU)
// ktab   = narrayU nhashU karray* khash*
// karray = ktabk
// khash  = ktabk ktabk
// ktabk  = ktabtypeU { intU | (loU hiU) | strB* }
//
// B = 8 bit, H = 16 bit, W = 32 bit, U = ULEB128 of W, U0/U1 = ULEB128 of W+1

// see:
//
//	* http://scm.zoomquiet.top/data/20131216145900/index.html
//	* https://github.com/LuaJIT/LuaJIT/blob/v2.1/src/lj_bcdump.h

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/text/encoding"

	"github.com/wader/fq/format"
	"github.com/wader/fq/pkg/decode"
	"github.com/wader/fq/pkg/interp"
	"github.com/wader/fq/pkg/scalar"
)

func init() {
	interp.RegisterFormat(
		format.LuaJIT,
		&decode.Format{
			Description: "LuaJIT 2.0 bytecode dump",
			DecodeFn:    LuaJITDecode,
		})
}

// reinterpret an int as a float
func u64tof64(u uint64) float64 {
	var buf [8]byte

	binary.BigEndian.PutUint64(buf[:], u)

	var f float64
	binary.Read(bytes.NewBuffer(buf[:]), binary.BigEndian, &f)

	return f
}

type DumpInfo struct {
	Strip     bool
	BigEndian bool
}

func LuaJITDecodeHeader(di *DumpInfo, d *decode.D) {
	d.FieldRawLen("magic", 3*8, d.AssertBitBuf([]byte{0x1b, 0x4c, 0x4a})) // ESC 'L' 'J'

	d.FieldU8("version")

	var flags uint64
	d.FieldStruct("flags", func(d *decode.D) {
		flags = d.FieldULEB128("raw")

		d.FieldValueBool("be", flags&0x01 > 0)
		d.FieldValueBool("strip", flags&0x02 > 0)
		d.FieldValueBool("ffi", flags&0x04 > 0)
		d.FieldValueBool("fr2", flags&0x08 > 0)
	})

	di.Strip = flags&0x2 > 0
	di.BigEndian = flags&0x1 > 0

	if !di.Strip {
		namelen := d.FieldU8("namelen")
		d.FieldStr("name", int(namelen), encoding.Nop)
	}
}

type jumpBias struct{}

func (j *jumpBias) MapUint(u scalar.Uint) (scalar.Uint, error) {
	u.Actual -= 0x8000
	return u, nil
}

func LuaJITDecodeBCIns(d *decode.D) {
	op := d.FieldU8("op", bcOpSyms)

	d.FieldU8("a")

	if opcodes[int(op)].HasD() {
		if opcodes[int(op)].IsJump() {
			d.FieldU16("j", &jumpBias{})
		} else {
			d.FieldU16("d")
		}
	} else {
		d.FieldU8("c")
		d.FieldU8("b")
	}
}

type ktabType struct{}

func (t *ktabType) MapUint(u scalar.Uint) (scalar.Uint, error) {
	switch u.Actual {
	case 0:
		u.Sym = "nil"
	case 1:
		u.Sym = "false"
	case 2:
		u.Sym = "true"
	case 3:
		u.Sym = "int"
	case 4:
		u.Sym = "num"
	default:
		u.Sym = "str"
	}
	return u, nil
}

func LuaJITDecodeKTabK(d *decode.D) {
	ktabtype := d.FieldULEB128("ktabtype", &ktabType{})

	if ktabtype >= 5 {
		sz := ktabtype - 5
		d.FieldStr("str", int(sz), encoding.Nop)
	} else {
		switch ktabtype {
		case 3:
			d.FieldULEB128("int")

		case 4:
			d.FieldAnyFn("num", func(d *decode.D) any {
				lo := d.ULEB128()
				hi := d.ULEB128()
				return u64tof64((hi << 32) + lo)
			})
		}
	}
}

func LuaJITDecodeCplx(d *decode.D) any {
	lo := d.ULEB128()
	if lo&1 == 0 {
		return lo >> 1
	} else {
		hi := d.ULEB128()
		return u64tof64((hi << 32) + (lo >> 1))
	}
}

type kgcType struct{}

func (t *kgcType) MapUint(u scalar.Uint) (scalar.Uint, error) {
	switch u.Actual {
	case 0:
		u.Sym = "child"
	case 1:
		u.Sym = "tab"
	case 2:
		u.Sym = "i64"
	case 3:
		u.Sym = "u64"
	case 4:
		u.Sym = "complex"
	default:
		u.Sym = "str"
	}
	return u, nil
}

func LuaJITDecodeKGC(d *decode.D) {
	kgctype := d.FieldULEB128("type", &kgcType{})

	if kgctype >= 5 {
		sz := kgctype - 5
		d.FieldStr("str", int(sz), encoding.Nop)
	} else {
		switch kgctype {
		case 0:
			//child

		case 1:
			// tab
			narray := d.FieldULEB128("narray")
			nhash := d.FieldULEB128("nhash")

			d.FieldArray("karray", func(d *decode.D) {
				for i := uint64(0); i < narray; i++ {
					d.FieldStruct("ktab", LuaJITDecodeKTabK)
				}
			})

			d.FieldArray("khash", func(d *decode.D) {
				for i := uint64(0); i < nhash; i++ {
					d.FieldStruct("khash", func(d *decode.D) {
						d.FieldStruct("k", LuaJITDecodeKTabK)
						d.FieldStruct("v", LuaJITDecodeKTabK)
					})
				}
			})

		case 2:
			d.FieldAnyFn("i64", func(d *decode.D) any {
				lo := d.ULEB128()
				hi := d.ULEB128()
				return int64((hi << 32) + lo)
			})

		case 3:
			d.FieldAnyFn("u64", func(d *decode.D) any {
				lo := d.ULEB128()
				hi := d.ULEB128()
				return (hi << 32) + lo
			})

		case 4:
			// complex

			d.FieldAnyFn("real", func(d *decode.D) any {
				rlo := d.ULEB128()
				rhi := d.ULEB128()
				r := (rhi << 32) + rlo
				return u64tof64(r)
			})

			d.FieldAnyFn("imag", func(d *decode.D) any {
				ilo := d.ULEB128()
				ihi := d.ULEB128()
				i := (ihi << 32) + ilo
				return u64tof64(i)
			})
		}
	}
}

func LuaJITDecodeKNum(d *decode.D) any {
	lo := d.ULEB128()
	if lo&1 == 0 {
		return lo >> 1
	} else {
		hi := d.ULEB128()
		return u64tof64((hi << 32) + (lo >> 1))
	}
}

func LuaJITDecodeProto(di *DumpInfo, d *decode.D) {
	length := d.FieldULEB128("length")

	d.LimitedFn(8*int64(length), func(d *decode.D) {
		d.FieldStruct("pdata", func(d *decode.D) {
			var numuv uint64
			var numkgc uint64
			var numkn uint64
			var numbc uint64
			var debuglen uint64

			d.FieldStruct("phead", func(d *decode.D) {
				d.FieldU8("flags")
				d.FieldU8("numparams")
				d.FieldU8("framesize")
				numuv = d.FieldU8("numuv")
				numkgc = d.FieldULEB128("numkgc")
				numkn = d.FieldULEB128("numkn")
				numbc = d.FieldULEB128("numbc")

				debuglen = 0
				if !di.Strip {
					debuglen = d.FieldULEB128("debuglen")
					if debuglen > 0 {
						d.FieldULEB128("firstline")
						d.FieldULEB128("numline")
					}
				}
			})

			d.FieldArray("bcins", func(d *decode.D) {
				for i := uint64(0); i < numbc; i++ {
					d.FieldStruct("ins", func(d *decode.D) {
						LuaJITDecodeBCIns(d)
					})
				}
			})

			d.FieldArray("uvdata", func(d *decode.D) {
				for i := uint64(0); i < numuv; i++ {
					d.FieldU16("uv")
				}
			})

			d.FieldArray("kgc", func(d *decode.D) {
				for i := uint64(0); i < numkgc; i++ {
					d.FieldStruct("kgc", LuaJITDecodeKGC)
				}
			})

			d.FieldArray("knum", func(d *decode.D) {
				for i := uint64(0); i < numkn; i++ {
					d.FieldAnyFn("knum", LuaJITDecodeKNum)
				}
			})

			if !di.Strip {
				d.FieldArray("debug", func(d *decode.D) {
					for i := uint64(0); i < debuglen; i++ {
						d.FieldU8("db")
					}
				})
			}
		})
	})
}

func LuaJITDecode(d *decode.D) any {
	di := DumpInfo{}

	d.FieldStruct("header", func(d *decode.D) {
		LuaJITDecodeHeader(&di, d)
	})

	if di.BigEndian {
		d.Endian = decode.BigEndian
	} else {
		d.Endian = decode.LittleEndian
	}

	d.FieldArray("proto", func(d *decode.D) {
		for {
			nextByte := d.PeekBytes(1)
			if bytes.Equal(nextByte, []byte{0}) {
				break
			}

			d.FieldStruct("proto", func(d *decode.D) {
				LuaJITDecodeProto(&di, d)
			})
		}

	})

	d.FieldU8("end")

	return nil
}
