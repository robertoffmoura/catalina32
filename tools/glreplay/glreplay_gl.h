// The OpenGL headers viewer/C32GL.m's decoding is compiled against in glreplay (Mesa's), with
// the Apple names it uses.
#define GL_GLEXT_PROTOTYPES 1
#include <GL/gl.h>
#include <GL/glext.h>

#ifndef GL_UNPACK_CLIENT_STORAGE_APPLE
#define GL_UNPACK_CLIENT_STORAGE_APPLE 0x85B2
#endif

/* The window's shader source calls go through glreplay (glreplay.c), which can change them to
   behave like another driver (GLREPLAY_CLIP). */
void glreplayShaderSource(GLhandleARB shader, GLsizei count, const GLcharARB **strings, const GLint *lengths);
#define glShaderSourceARB glreplayShaderSource
const GLubyte *glreplayGetString(GLenum name);
#define glGetString glreplayGetString
