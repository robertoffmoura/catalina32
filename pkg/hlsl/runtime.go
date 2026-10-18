package hlsl

// Programs run Lanes invocations (vertices) at once: every value slot holds one float per lane,
// so each compiled operation loops over lanes and the per-operation dispatch cost is shared.
// Divergent control flow uses execution masks: an if runs each branch for the lanes that take
// it, a return retires its lanes, and stores to variables only touch executing lanes.

// Lanes is the number of invocations a State evaluates together.
const Lanes = 32

// Lane holds one value slot across all invocations.
type Lane = [Lanes]float32

const allLanes = 1<<Lanes - 1

// ctx is the evaluation context: memory and the lanes executing the current statement.
type ctx struct {
	m    []Lane
	mask uint32 // lanes executing
	retd uint32 // lanes that have returned from the current function
}

type evalFn func(x *ctx)

func noop(x *ctx) {}

// laneBits is the set of lanes where c is nonzero.
func laneBits(c *Lane) uint32 {
	var t uint32
	for l, v := range c {
		if v != 0 {
			t |= 1 << l
		}
	}
	return t
}

// storeMasked copies n slots from src to dst in the lanes of mask.
func storeMasked(m []Lane, dst, src, n int, mask uint32) {
	if mask == allLanes {
		copy(m[dst:dst+n], m[src:src+n])
		return
	}
	for i := 0; i < n; i++ {
		d, s := &m[dst+i], &m[src+i]
		for l := range d {
			if mask>>l&1 != 0 {
				d[l] = s[l]
			}
		}
	}
}

// State is the evaluation memory for one program; use one State per goroutine. M[slot][lane]
// holds inputs (see Program.Inputs) before Run and outputs after it.
type State struct {
	P     *Program
	M     []Lane
	loads []constLoad
	x     ctx
}

// constLoad copies one float from constant register reg (component comp) to slot dst; sampler
// constants store their register index instead (comp < 0).
type constLoad struct {
	dst, reg, comp int
}

// NewState allocates evaluation memory and precomputes how constants are loaded.
func (p *Program) NewState() *State {
	s := &State{P: p, M: make([]Lane, p.MemSize)}
	s.x.m = s.M
	for _, k := range p.Constants {
		s.loads = appendLoads(s.loads, k.Type, k.off, k.RegisterIndex, k.RegisterSet == RegSampler)
	}
	for _, l := range p.lits {
		fill(&s.M[l.off], l.v)
	}
	return s
}

func fill(d *Lane, v float32) {
	for l := range d {
		d[l] = v
	}
}

// appendLoads maps a uniform of type t at slot off onto registers starting at reg, using D3D9's
// column-major packing: matrix element [r][c] lives in register reg+c, component r.
func appendLoads(l []constLoad, t *Type, off, reg int, sampler bool) []constLoad {
	switch {
	case t.IsArray():
		for i := 0; i < t.ArrayLen; i++ {
			l = appendLoads(l, t.Elem, off+i*t.Elem.Size(), reg+i*registerCount(t.Elem), sampler)
		}
	case t.Base == BSampler || sampler:
		l = append(l, constLoad{dst: off, reg: reg, comp: -1})
	case t.Base == BStruct:
		for _, f := range t.Struct.Fields {
			l = appendLoads(l, f.Type, off, reg, false)
			off += f.Type.Size()
			reg += registerCount(f.Type)
		}
	case t.Rows > 1:
		for r := 0; r < t.Rows; r++ {
			for c := 0; c < t.Cols; c++ {
				l = append(l, constLoad{dst: off + r*t.Cols + c, reg: reg + c, comp: r})
			}
		}
	default:
		for c := 0; c < t.Cols; c++ {
			l = append(l, constLoad{dst: off + c, reg: reg, comp: c})
		}
	}
	return l
}

// LoadConstants copies uniform values from float constant registers into every lane
// (out-of-range registers read as zero).
func (s *State) LoadConstants(regs [][4]float32) {
	m := s.M
	for _, ld := range s.loads {
		var v float32
		switch {
		case ld.comp < 0:
			v = float32(ld.reg)
		case ld.reg < len(regs):
			v = regs[ld.reg][ld.comp]
		}
		fill(&m[ld.dst], v)
	}
	s.x.mask, s.x.retd = allLanes, 0
	for _, f := range s.P.pre {
		f(&s.x)
	}
}

// Run executes the entry point for lanes 0..n-1 with the inputs currently stored in their
// input slots.
func (s *State) Run(n int) {
	if n <= 0 {
		return
	}
	x := &s.x
	for _, t := range s.P.TexCalls {
		s.M[t.flagOff] = Lane{}
	}
	x.mask, x.retd = allLanes, 0
	for _, f := range s.P.inits {
		f(x)
	}
	x.mask = allLanes >> (Lanes - min(n, Lanes))
	s.P.run(x)
}

// TexCoord returns the coordinates lane passed to the first texture lookup (in code order) that
// it executed during the last Run on sampler register unit.
func (s *State) TexCoord(unit, lane int) (coord [4]float32, n int, ok bool) {
	for _, t := range s.P.TexCalls {
		if s.M[t.flagOff][lane] != 0 && int(s.M[t.unitOff][lane]) == unit {
			n = min(t.coordN, 4)
			for i := 0; i < n; i++ {
				coord[i] = s.M[t.coordOff+i][lane]
			}
			return coord, n, true
		}
	}
	return coord, 0, false
}

// Input returns the input element with the given semantic and index, or nil.
func (p *Program) Input(semantic string, index int) *Varying {
	return findVarying(p.Inputs, semantic, index)
}

// Output returns the output element with the given semantic and index, or nil.
func (p *Program) Output(semantic string, index int) *Varying {
	return findVarying(p.Outputs, semantic, index)
}

func findVarying(l []*Varying, semantic string, index int) *Varying {
	for _, v := range l {
		if v.Semantic == semantic && v.Index == index {
			return v
		}
	}
	return nil
}
