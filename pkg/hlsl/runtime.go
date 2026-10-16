package hlsl

// State is the evaluation memory for one program; use one State per goroutine.
type State struct {
	P     *Program
	M     []float32
	loads []constLoad
}

// constLoad copies one float from constant register reg (component comp) to slot dst; sampler
// constants store their register index instead (comp < 0).
type constLoad struct {
	dst, reg, comp int
}

// NewState allocates evaluation memory and precomputes how constants are loaded.
func (p *Program) NewState() *State {
	s := &State{P: p, M: make([]float32, p.MemSize)}
	for _, k := range p.Constants {
		s.loads = appendLoads(s.loads, k.Type, k.off, k.RegisterIndex, k.RegisterSet == RegSampler)
	}
	return s
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

// LoadConstants copies uniform values from float constant registers (out-of-range registers
// read as zero).
func (s *State) LoadConstants(regs [][4]float32) {
	m := s.M
	for _, ld := range s.loads {
		switch {
		case ld.comp < 0:
			m[ld.dst] = float32(ld.reg)
		case ld.reg < len(regs):
			m[ld.dst] = regs[ld.reg][ld.comp]
		default:
			m[ld.dst] = 0
		}
	}
}

// Run executes the entry point with the inputs currently stored in the input slots.
func (s *State) Run() {
	for _, f := range s.P.inits {
		f(s.M)
	}
	s.P.run(s.M)
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
