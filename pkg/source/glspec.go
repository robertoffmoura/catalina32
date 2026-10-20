package source

import "strings"

// The OpenGL calls the game makes (ToGL and the launcher), as the stream to the viewer carries
// them. One table drives the emulator's encoder (glremote.go) and, through cmd/glgen, the
// viewer's decoder (viewer/C32GLCalls.h). Changing it changes the protocol: run
// `go generate ./pkg/source`, which also bumps nothing by itself — bump glVersion.
//
// An entry is "name: args", args separated by spaces:
//
//	i      32-bit integer (GLenum, GLint, GLuint, GLsizei, GLboolean, GLbitfield)
//	l      GLintptr / GLsizeiptr: 32 bits in the game, pointer-sized in the viewer
//	f d    float, double
//	T B F R Q P H   an object name: texture, buffer, framebuffer, renderbuffer, query, ARB
//	       assembly program, GLSL object (GLhandleARB). The game's names are its own; the viewer
//	       maps them to real ones (framebuffer 0 is the game's window).
//	gX     names to create (count = the previous argument), of kind X; xX: names to delete
//	s      a C string
//	*N@k   N * argument k bytes of data (*N: N bytes); the pointer may be NULL
//	p      data whose size the emulator works out (pixels, indices, buffer contents) or an
//	       offset into a bound buffer
//
// Flags after the name: E the emulator encodes it by hand; V the viewer decodes it by hand.
// Names starting with c32 are not GL functions: presenting, the window size, and the questions
// the emulator waits for an answer to (the first argument is the reply token; see glremote.go).
// Calls answered in the emulator (glGetError, fences, compile status...) aren't listed.

//go:generate go run ../../cmd/glgen

// GLVersion is the stream protocol version (the viewer checks it).
const GLVersion = 1

// GLSpec lists the calls; a call's position is its id on the wire.
var GLSpec = strings.Split(strings.TrimSpace(`
glActiveTexture: i
glClientActiveTexture: i
glAlphaFunc: i f
glAttachObjectARB: H H
glDetachObjectARB: H H
glBegin: i
glEnd:
glBeginQueryARB V: i Q
glEndQueryARB V: i
glBindAttribLocationARB: H i s
glBindBufferARB E: i B
glBindBuffer E: i B
glBindFramebuffer V: i F
glBindFramebufferEXT V: i F
glBindProgramARB: i P
glBindRenderbuffer: i R
glBindRenderbufferEXT: i R
glBindTexture: i T
glBlendColor: f f f f
glBlendEquation: i
glBlendEquationSeparate: i i
glBlendFunc: i i
glBlendFuncSeparate: i i i i
glBlitFramebuffer: i i i i i i i i i i
glBlitFramebufferEXT: i i i i i i i i i i
glBufferDataARB E: i l p i
glBufferData E: i l p i
glBufferSubDataARB: i l l *1@2
glBufferSubData: i l l *1@2
glBufferParameteriAPPLE E: i i i
glClear: i
glClearColor: f f f f
glClearDepth: d
glClearStencil: i
glClipPlane: i *32
glColor4f: f f f f
glColor4ub: i i i i
glColor4sv: *8
glColorMask: i i i i
glColorMaskIndexedEXT: i i i i i
glCompileShaderARB: H
glCompressedTexImage2D E: i i i i i i i p
glCompressedTexImage2DARB E: i i i i i i i p
glCompressedTexImage3D E: i i i i i i i i p
glCompressedTexSubImage2D E: i i i i i i i i p
glCreateProgramObjectARB EV: i
glCreateShaderObjectARB EV: i i
glDeleteObjectARB: H
glDeleteShader: H
glDeleteProgram: H
glDeleteBuffersARB: i xB
glDeleteBuffers: i xB
glDeleteFramebuffers: i xF
glDeleteFramebuffersEXT: i xF
glDeleteProgramsARB: i xP
glDeleteQueriesARB: i xQ
glDeleteRenderbuffers: i xR
glDeleteRenderbuffersEXT: i xR
glDeleteTextures: i xT
glGenBuffersARB E: i gB
glGenBuffers E: i gB
glGenFramebuffers E: i gF
glGenFramebuffersEXT E: i gF
glGenProgramsARB E: i gP
glGenQueriesARB E: i gQ
glGenRenderbuffers E: i gR
glGenRenderbuffersEXT E: i gR
glGenTextures E: i gT
glDepthFunc: i
glDepthMask: i
glDepthRange: d d
glDisable: i
glEnable: i
glDisableClientState: i
glEnableClientState: i
glDisableIndexedEXT: i i
glEnableIndexedEXT: i i
glDisableVertexAttribArray: i
glDisableVertexAttribArrayARB: i
glEnableVertexAttribArray: i
glEnableVertexAttribArrayARB: i
glDrawArrays E: i i i
glDrawBuffer V: i
glReadBuffer V: i
glDrawElements E: i i i p
glDrawRangeElements E: i i i i i p
glFinish:
glFlush:
glFramebufferRenderbuffer: i i i R
glFramebufferRenderbufferEXT: i i i R
glFramebufferTexture2D: i i i T i
glFramebufferTexture2DEXT: i i i T i
glFramebufferTexture3D: i i i T i i
glFramebufferTexture3DEXT: i i i T i i
glFrontFace: i
glHint: i i
glLineWidth: f
glPointSize: f
glLinkProgramARB V: H
glMatrixMode: i
glLoadIdentity:
glLoadMatrixf: *64
glOrtho: d d d d d d
glPixelStorei EV: i i
glPolygonMode: i i
glPolygonOffset: f f
glPopAttrib:
glPopClientAttrib:
glPushAttrib: i
glPushClientAttrib: i
glProgramEnvParameters4fvEXT: i i i *16@2
glProgramEnvParameter4fARB: i i f f f f
glProgramLocalParameter4fARB: i i f f f f
glProgramStringARB: i i i *1@2
glRenderbufferStorage: i i i i
glRenderbufferStorageEXT: i i i i
glRenderbufferStorageMultisample: i i i i i
glRenderbufferStorageMultisampleEXT: i i i i i
glSampleCoverage: f i
glScissor: i i i i
glShaderSourceARB E: H s
glStencilFunc: i i i
glStencilFuncSeparate: i i i i
glStencilMask: i
glStencilMaskSeparate: i i
glStencilOp: i i i
glStencilOpSeparate: i i i i
glActiveStencilFaceEXT: i
glTexCoord2f: f f
glTexCoord3f: f f f
glVertex2f: f f
glVertex3f: f f f
glColor3f: f f f
glTexEnvi: i i i
glTexEnvf: i i f
glTexImage2D E: i i i i i i i i p
glTexImage3D E: i i i i i i i i i p
glTexSubImage2D E: i i i i i i i i p
glTexSubImage3D E: i i i i i i i i i i p
glTexParameteri: i i i
glTexParameterf: i i f
glTexParameterfv: i i *16
glTexParameteriv: i i *16
glUniform1f: i f
glUniform2f: i f f
glUniform3f: i f f f
glUniform4f: i f f f f
glUniform1i: i i
glUniform1iARB: i i
glUniform1fv: i i *4@1
glUniform2fv: i i *8@1
glUniform3fv: i i *12@1
glUniform4fv: i i *16@1
glUniform4fvARB: i i *16@1
glUniform1iv: i i *4@1
glUniformMatrix4fv: i i i *64@1
glUniformBufferEXT: H i B
glUseProgram: H
glUseProgramObjectARB: H
glVertexAttribPointer E: i i i i i p
glVertexAttribPointerARB E: i i i i i p
glVertexAttrib4f: i f f f f
glVertexAttrib4fv: i *16
glVertexPointer E: i i i p
glTexCoordPointer E: i i i p
glColorPointer E: i i i p
glViewport: i i i i
c32Present V: i
c32ClientArray V: i i i i i i i *1@6
c32WindowSize V: i i
c32MouseMode V: i
c32GetString V: i i
c32GetValues V: i i i
c32IsEnabled V: i i
c32GetTexLevelParameter V: i i i i
c32GetTexImage V: i i i i i i
c32ReadPixels V: i i i i i i i
c32UniformLocation V: i H s
c32AttribLocation V: i H s
`), "\n")
