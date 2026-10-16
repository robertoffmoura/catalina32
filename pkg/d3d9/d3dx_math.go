package d3d9

import "catalina32/pkg/cpu"

// thunkD3DXMatrixMultiply implements D3DXMATRIX* D3DXMatrixMultiply(D3DXMATRIX *pOut, const D3DXMATRIX *pM1, const D3DXMATRIX *pM2).
func thunkD3DXMatrixMultiply(c *cpu.CPU) {
	pOut := c.Arg(0)
	pM1 := c.Arg(1)
	pM2 := c.Arg(2)
	var a, b, out [16]float32
	for i := 0; i < 16; i++ {
		a[i] = c.ReadF32(pM1 + uint32(i*4))
		b[i] = c.ReadF32(pM2 + uint32(i*4))
	}
	for r := 0; r < 4; r++ {
		for col := 0; col < 4; col++ {
			out[r*4+col] = a[r*4+0]*b[0*4+col] +
				a[r*4+1]*b[1*4+col] +
				a[r*4+2]*b[2*4+col] +
				a[r*4+3]*b[3*4+col]
		}
	}
	for i := 0; i < 16; i++ {
		c.WriteF32(pOut+uint32(i*4), out[i])
	}
	c.Regs[cpu.RegEAX] = pOut
	c.Ret()
}
