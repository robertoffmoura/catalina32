// The OpenGL headers viewer/C32GL.m's decoding is compiled against in glreplay (Mesa's), with
// the Apple names it uses.
#define GL_GLEXT_PROTOTYPES 1
#include <GL/gl.h>
#include <GL/glext.h>

#ifndef GL_UNPACK_CLIENT_STORAGE_APPLE
#define GL_UNPACK_CLIENT_STORAGE_APPLE 0x85B2
#endif
