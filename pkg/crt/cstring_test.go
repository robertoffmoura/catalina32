package crt

import (
	"math"
	"testing"

	"catalina32/pkg/cpu"
)

func TestStrtol(t *testing.T) {
	for _, tc := range []struct {
		in   string
		base int
		want int32
		end  int
	}{
		{"12abc", 10, 12, 2},
		{"  -42 rest", 10, -42, 5},
		{"0x1F", 0, 31, 4},
		{"0x1F", 16, 31, 4},
		{"0xg", 0, 0, 1}, // "0" then 'x' is not part of the number
		{"017", 0, 15, 3},
		{"abc", 10, 0, 0},
		{"+", 10, 0, 0},
		{"99999999999", 10, math.MaxInt32, 11},
		{"-99999999999", 10, math.MinInt32, 12},
		{"zz", 36, 35*36 + 35, 2},
	} {
		v, end := strtol32([]byte(tc.in), tc.base)
		if v != tc.want || end != tc.end {
			t.Errorf("strtol(%q, %d) = %d, end %d; want %d, end %d", tc.in, tc.base, v, end, tc.want, tc.end)
		}
	}
	if v, _ := strtoul32([]byte("-1"), 10); v != math.MaxUint32 {
		t.Errorf("strtoul(-1) = %d", v)
	}
	if v, _ := strtoul32([]byte("4294967296"), 10); v != math.MaxUint32 {
		t.Errorf("strtoul overflow = %d", v)
	}
}

func TestParseFloat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		end  int
	}{
		{"1.5", 1.5, 3},
		{" -2.5e3x", -2500, 7},
		{"3.", 3, 2},
		{".25f", 0.25, 3},
		{"1e", 1, 1},
		{"0x10", 16, 4},
		{"inf", math.Inf(1), 3},
		{"abc", 0, 0},
		{".", 0, 0},
	} {
		v, end := parseFloat([]byte(tc.in))
		if v != tc.want || end != tc.end {
			t.Errorf("strtod(%q) = %v, end %d; want %v, end %d", tc.in, v, end, tc.want, tc.end)
		}
	}
}

func TestCompareFold(t *testing.T) {
	if compareFold([]byte("Hello"), []byte("hELLO")) != 0 || compareFold([]byte("a"), []byte("B")) != -1 || compareFold([]byte("ab"), []byte("A")) != 1 {
		t.Error("compareFold")
	}
}

func TestSscanf(t *testing.T) {
	c := cpu.NewCPU()
	const out, va = 0x100000, 0x200000
	ptrs := []uint32{out, out + 16, out + 32, out + 64}
	for i, p := range ptrs {
		c.Write32(va+uint32(4*i), p)
	}
	scan := func(in, format string) int32 {
		return scanGuest(c, []byte(in), []byte(format), va)
	}

	if n := scan("1,2", "%d,%d"); n != 2 || c.Read32(out) != 1 || c.Read32(out+16) != 2 {
		t.Errorf("%%d,%%d: n=%d %d %d", n, c.Read32(out), c.Read32(out+16))
	}
	if n := scan("3.25 name rest", "%lf %s %3c"); n != 3 || c.ReadF64(out) != 3.25 || c.ReadCString(out+16) != "name" || string(c.Mem[out+32:out+35]) != "res" {
		t.Errorf("%%lf %%s %%3c: n=%d %v %q", n, c.ReadF64(out), c.ReadCString(out+16))
	}
	if n := scan("0.5", "%f"); n != 1 || c.ReadF32(out) != 0.5 {
		t.Errorf("%%f: n=%d %v", n, c.ReadF32(out))
	}
	if n := scan("key=value;", "%[^=]=%[^;]%n"); n != 2 || c.ReadCString(out) != "key" || c.ReadCString(out+16) != "value" || c.Read32(out+32) != 9 {
		t.Errorf("scanset: n=%d %q %q %d", n, c.ReadCString(out), c.ReadCString(out+16), c.Read32(out+32))
	}
	if n := scan("ff 7", "%x %*d"); n != 1 || c.Read32(out) != 255 {
		t.Errorf("%%x %%*d: n=%d %d", n, c.Read32(out))
	}
	if n := scan("", "%d"); n != -1 {
		t.Errorf("empty input: n=%d, want -1", n)
	}
	if n := scan("x", "%d"); n != 0 {
		t.Errorf("mismatch: n=%d, want 0", n)
	}
}

func TestMemset(t *testing.T) {
	const stack, buf = 0x300000, 0x100000
	call := func(c *cpu.CPU, dst, val, n uint32) {
		c.Regs[cpu.RegESP] = stack
		c.Write32(stack, 0xDEAD) // return address
		c.Write32(stack+4, dst)
		c.Write32(stack+8, val)
		c.Write32(stack+12, n)
		thunkMemset(c)
		if c.Regs[cpu.RegEAX] != dst || c.EIP != 0xDEAD {
			t.Fatalf("memset(%#x, %#x, %d) returned %#x to %#x", dst, val, n, c.Regs[cpu.RegEAX], c.EIP)
		}
	}
	for _, tc := range []struct {
		val uint32
		n   int
	}{{0, 0}, {0xAB, 0}, {0xAB, 1}, {0, 1}, {0x5C, 1000}, {0, 1000}, {0x1FF, 37}} { // 0x1FF: only the low byte counts
		c := cpu.NewCPU()
		for i := 0; i < 1100; i++ {
			c.Mem[buf-50+i] = 0x77
		}
		call(c, buf, tc.val, uint32(tc.n))
		for i := -50; i < 1050; i++ {
			want := byte(0x77)
			if i >= 0 && i < tc.n {
				want = byte(tc.val)
			}
			if got := c.Mem[buf+i]; got != want {
				t.Fatalf("memset(val %#x, n %d): byte %d = %#x, want %#x", tc.val, tc.n, i, got, want)
			}
		}
	}

	// A range past the end of guest memory writes nothing and halts the guest.
	c := cpu.NewCPU()
	end := uint32(len(c.Mem))
	c.Mem[end-1] = 0x77
	call(c, end-4, 0xAB, 16)
	if !c.Halted || c.Mem[end-1] != 0x77 {
		t.Fatalf("out-of-range memset: halted %v, last byte %#x", c.Halted, c.Mem[end-1])
	}
}
