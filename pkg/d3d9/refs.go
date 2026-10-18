package d3d9

import "catalina32/pkg/cpu"

// Reference counting for the resources the device creates (textures, surfaces, vertex and index
// buffers), following D3D9: an object starts with one reference, the device holds one for each
// binding (SetTexture, SetStreamSource, SetIndices, SetRenderTarget), Get* methods that return
// an object add one, and the child surfaces of a texture (GetSurfaceLevel, GetCubeMapSurface)
// forward their references to it. Releasing the last reference frees the object's guest memory,
// so loading level after level does not exhaust the guest heap.

type comRef struct {
	count    int
	parent   uint32            // container of a child surface (its references are the container's)
	children map[uint32]uint32 // child surfaces by level/face key
	alias    bool              // shares its container's BufferDesc
}

// tracked reports whether objects with this vtable are reference counted.
func (s *Subsystem) tracked(vt uint32) bool {
	switch vt {
	case s.vtTexture9, s.vtCubeTex9, s.vtVolTex9, s.vtSurface9, s.vtVertexBuf9, s.vtIndexBuf9:
		return true
	}
	return false
}

// root returns the object that holds obj's references.
func (s *Subsystem) root(obj uint32) *comRef {
	r := s.refs[obj]
	if r != nil && r.parent != 0 {
		return s.refs[r.parent]
	}
	return r
}

// addRef adds a reference and returns the new count (objects that are not tracked report 2).
func (s *Subsystem) addRef(obj uint32) int {
	r := s.root(obj)
	if r == nil {
		return 2
	}
	r.count++
	return r.count
}

// release drops a reference, destroying the object at zero, and returns the new count.
func (s *Subsystem) release(c *cpu.CPU, obj uint32) int {
	r := s.root(obj)
	if r == nil {
		return 1
	}
	if r.count <= 0 {
		return 0
	}
	r.count--
	if r.count == 0 {
		if s.refs[obj].parent != 0 {
			obj = s.refs[obj].parent
		}
		s.destroy(c, obj)
	}
	return r.count
}

// bind replaces a device binding, moving the device's reference from the old object to the new.
func (s *Subsystem) bind(c *cpu.CPU, slot *uint32, obj uint32) {
	if *slot == obj {
		return
	}
	s.addRef(obj)
	old := *slot
	*slot = obj
	s.release(c, old)
}

// child returns the child surface of container cont under key, creating it on first use, and
// adds a reference to the container for the caller.
func (s *Subsystem) child(c *cpu.CPU, cont uint32, key uint32, create func() uint32) uint32 {
	r := s.refs[cont]
	if r == nil {
		return create() // untracked container: a standalone surface
	}
	surf, ok := r.children[key]
	if !ok {
		surf = create()
		if r.children == nil {
			r.children = map[uint32]uint32{}
		}
		r.children[key] = surf
		cr := s.refs[surf]
		cr.parent = cont
		cr.alias = s.Resources[surf] == s.Resources[cont]
	}
	r.count++
	return surf
}

// destroy frees obj, its children and their guest memory.
func (s *Subsystem) destroy(c *cpu.CPU, obj uint32) {
	r := s.refs[obj]
	for _, ch := range r.children {
		s.destroy(c, ch)
	}
	if res := s.Resources[obj]; res != nil && !r.alias {
		if res.DataPtr != 0 {
			c.Free(res.DataPtr)
		}
		delete(s.renderTargets, res)
	}
	delete(s.Resources, obj)
	delete(s.refs, obj)
	delete(s.origin, obj)
	delete(s.dumpedTex, obj)
	c.Free(obj)
}
