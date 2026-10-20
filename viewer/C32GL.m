// OpenGL renderer for a Source game's GL stream (see C32GL.h). Compiled with ARC.
#define GL_SILENCE_DEPRECATION 1
#pragma clang diagnostic ignored "-Wdeprecated-declarations" /* NSOpenGL: deprecated with OpenGL, still there */
#import "C32GL.h"
#import <OpenGL/OpenGL.h>
#import <OpenGL/gl.h>
#import <OpenGL/glext.h>
#include <dlfcn.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <unistd.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Shared memory layout: pkg/source/glremote.go. */
#define kMagic 0x4C323343u /* "C32L" */
#define kOffReady 24
#define kOffErrors 28
#define kOffWrite 32
#define kOffRead 40
#define kOffReplyToken 48
#define kOffReplyLen 52
#define kOffFrames 56
#define kOffScreenW 64
#define kOffScreenH 68
#define kOffState 72
#define kOffEventWrite 128
#define kOffEventRead 132
#define kEventsOffset 4096
#define kEvents 1024
#define kEventSize 64
#define kQueriesOffset (128 << 10)
#define kQueries 16384
#define kSkip 0xFFFFFFFFu

enum { evKeyDown = 1, evKeyUp, evMotion, evButtonDown, evButtonUp, evWheel, evText, evQuit, evFocusIn, evFocusOut };

/* ---- The decoder's state (decode thread only, except where noted) ---- */

typedef struct {
    uint32_t client, real;
} C32Pending;

typedef struct C32GLDecoder {
    uint8_t *shm;
    uint8_t *ring, *reply;
    uint32_t ringSize, replySize;
    uint32_t *names[128]; /* client name -> real name, by object kind letter */
    uint32_t caps[128];
    uintptr_t *handles;   /* GLSL objects ('H') */
    uint32_t hcap;
    uint32_t delBuf[4096];
    /* The game's window: an offscreen framebuffer. */
    GLuint virtFBO, virtColor, virtDepth;
    int virtW, virtH;
    GLuint drawFBO, readFBO; /* real framebuffers bound */
    uint32_t activeQuery[4];
    C32Pending *pending;
    int npending, pendingCap;
    void *arrays[104]; /* client vertex arrays' copies: 0..15 generic, 100.. fixed function */
    size_t arraySize[104];
    const char *lastCall;
    uint32_t errors;
} C32GLDecoder;

static _Atomic int g_virtW = 0, g_virtH = 0; /* for the view's coordinate mapping (main thread) */
static void c32SetMouseMode(int relative);

/* ---- Readers ---- */

static uint32_t c32RdU(const uint8_t **p) {
    uint32_t v;
    memcpy(&v, *p, 4);
    *p += 4;
    return v;
}
static float c32RdF(const uint8_t **p) {
    float v;
    memcpy(&v, *p, 4);
    *p += 4;
    return v;
}
static double c32RdD(const uint8_t **p) {
    double v;
    memcpy(&v, *p, 8);
    *p += 8;
    return v;
}
/* Strings and data: u32 length (0xFFFFFFFF: NULL), bytes padded to 4. */
static const void *c32RdBlob(const uint8_t **p) {
    uint32_t n = c32RdU(p);
    if (n == 0xFFFFFFFFu) return NULL;
    const void *v = *p;
    *p += (n + 3) & ~3u;
    return v;
}
static const char *c32RdStr(const uint8_t **p) { return (const char *)c32RdBlob(p); }
/* 'p' data: u32 mode (0 NULL, 1 offset, 2 data follows), u32 value. */
static const void *c32RdPixels(const uint8_t **p) {
    uint32_t mode = c32RdU(p), v = c32RdU(p);
    if (mode == 1) return (const void *)(uintptr_t)v;
    if (mode == 2) {
        const void *data = *p;
        *p += (v + 3) & ~3u;
        return data;
    }
    return NULL;
}
static const uint32_t *c32RdNames(const uint8_t **p, uint32_t n) {
    const uint32_t *v = (const uint32_t *)*p;
    *p += 4 * n;
    return v;
}

/* ---- Object names ---- */

static void genReal(char kind, GLsizei n, GLuint *out) {
    switch (kind) {
    case 'T': glGenTextures(n, out); break;
    case 'B': glGenBuffers(n, out); break;
    case 'F': glGenFramebuffersEXT(n, out); break;
    case 'R': glGenRenderbuffersEXT(n, out); break;
    case 'Q': glGenQueries(n, out); break;
    case 'P': glGenProgramsARB(n, out); break;
    }
}

static uint32_t *slot(C32GLDecoder *d, char kind, uint32_t name) {
    unsigned k = (unsigned char)kind;
    if (name >= d->caps[k]) {
        uint32_t cap = d->caps[k] ? d->caps[k] : 1024;
        while (cap <= name) cap *= 2;
        d->names[k] = realloc(d->names[k], cap * sizeof(uint32_t));
        memset(d->names[k] + d->caps[k], 0, (cap - d->caps[k]) * sizeof(uint32_t));
        d->caps[k] = cap;
    }
    return &d->names[k][name];
}

static uint32_t c32Obj(C32GLDecoder *d, char kind, uint32_t name) {
    if (name == 0) return kind == 'F' ? d->virtFBO : 0; /* framebuffer 0: the game's window */
    uint32_t *s = slot(d, kind, name);
    if (!*s) genReal(kind, 1, s); /* a name the game didn't generate first (allowed by these GL versions) */
    return *s;
}

static void *c32ObjH(C32GLDecoder *d, uint32_t name) {
    if (name == 0 || name >= d->hcap) return NULL;
    return (void *)d->handles[name];
}

static void setHandle(C32GLDecoder *d, uint32_t name, uintptr_t real) {
    if (name >= d->hcap) {
        uint32_t cap = d->hcap ? d->hcap : 1024;
        while (cap <= name) cap *= 2;
        d->handles = realloc(d->handles, cap * sizeof(uintptr_t));
        memset(d->handles + d->hcap, 0, (cap - d->hcap) * sizeof(uintptr_t));
        d->hcap = cap;
    }
    d->handles[name] = real;
}

static void c32GenNames(C32GLDecoder *d, char kind, uint32_t n, const uint32_t *names, void *fn) {
    (void)fn;
    for (uint32_t i = 0; i < n; i++) {
        uint32_t *s = slot(d, kind, names[i]);
        if (!*s) genReal(kind, 1, s);
    }
}

static const uint32_t *c32DelNames(C32GLDecoder *d, char kind, uint32_t n, const uint32_t *names) {
    if (n > 4096) n = 4096;
    for (uint32_t i = 0; i < n; i++) {
        uint32_t *s = slot(d, kind, names[i]);
        d->delBuf[i] = *s;
        *s = 0;
    }
    return d->delBuf;
}

/* ---- Replies ---- */

static void reply(C32GLDecoder *d, uint32_t token, const void *data, uint32_t n) {
    if (n > d->replySize) n = d->replySize;
    if (data && n) memcpy(d->reply, data, n);
    atomic_store_explicit((_Atomic uint32_t *)(d->shm + kOffReplyLen), n, memory_order_relaxed);
    atomic_store_explicit((_Atomic uint32_t *)(d->shm + kOffReplyToken), token, memory_order_release);
}

#include "C32GLCalls.h"

/* ---- Calls the viewer handles itself ---- */

static void c32gl_glBeginQueryARB(C32GLDecoder *d, uint32_t target, uint32_t q) {
    glBeginQuery(target, q);
    d->activeQuery[target & 3] = 0;
    /* (the client name is needed to post the result: find it) */
    for (uint32_t i = 1; i < d->caps['Q']; i++) {
        if (d->names['Q'][i] == q) {
            d->activeQuery[target & 3] = i;
            break;
        }
    }
}

static void c32gl_glEndQueryARB(C32GLDecoder *d, uint32_t target) {
    glEndQuery(target);
    uint32_t client = d->activeQuery[target & 3];
    if (!client) return;
    if (d->npending == d->pendingCap) {
        d->pendingCap = d->pendingCap ? 2 * d->pendingCap : 64;
        d->pending = realloc(d->pending, d->pendingCap * sizeof(C32Pending));
    }
    d->pending[d->npending++] = (C32Pending){client, d->names['Q'][client]};
}

/* Posts the results of finished queries. */
static void pollQueries(C32GLDecoder *d) {
    int kept = 0;
    for (int i = 0; i < d->npending; i++) {
        C32Pending q = d->pending[i];
        GLuint avail = 0;
        glGetQueryObjectuiv(q.real, GL_QUERY_RESULT_AVAILABLE, &avail);
        if (!avail) {
            d->pending[kept++] = q;
            continue;
        }
        GLuint result = 0;
        glGetQueryObjectuiv(q.real, GL_QUERY_RESULT, &result);
        uint8_t *e = d->shm + kQueriesOffset + 16 * (q.client % kQueries);
        atomic_store_explicit((_Atomic uint64_t *)(e + 8), (uint64_t)result, memory_order_relaxed);
        atomic_store_explicit((_Atomic uint32_t *)e, 1, memory_order_release);
    }
    d->npending = kept;
}

static void bindFB(C32GLDecoder *d, uint32_t target, uint32_t fbo) {
    glBindFramebufferEXT(target, fbo);
    if (target == GL_FRAMEBUFFER_EXT || target == GL_DRAW_FRAMEBUFFER_EXT) d->drawFBO = fbo;
    if (target == GL_FRAMEBUFFER_EXT || target == GL_READ_FRAMEBUFFER_EXT) d->readFBO = fbo;
}
static void c32gl_glBindFramebuffer(C32GLDecoder *d, uint32_t target, uint32_t fbo) { bindFB(d, target, fbo); }
static void c32gl_glBindFramebufferEXT(C32GLDecoder *d, uint32_t target, uint32_t fbo) { bindFB(d, target, fbo); }

/* The game's window is a framebuffer here: its back buffer is colour attachment 0. */
static GLenum windowBuffer(C32GLDecoder *d, GLuint fbo, GLenum mode) {
    if (fbo == d->virtFBO && (mode == GL_BACK || mode == GL_FRONT || mode == GL_BACK_LEFT || mode == GL_FRONT_LEFT)) {
        return GL_COLOR_ATTACHMENT0_EXT;
    }
    return mode;
}
static void c32gl_glDrawBuffer(C32GLDecoder *d, uint32_t mode) { glDrawBuffer(windowBuffer(d, d->drawFBO, mode)); }
static void c32gl_glReadBuffer(C32GLDecoder *d, uint32_t mode) { glReadBuffer(windowBuffer(d, d->readFBO, mode)); }

static void c32gl_glCreateProgramObjectARB(C32GLDecoder *d, uint32_t h) {
    setHandle(d, h, (uintptr_t)glCreateProgramObjectARB());
}

static void c32gl_glCreateShaderObjectARB(C32GLDecoder *d, uint32_t type, uint32_t h) {
    setHandle(d, h, (uintptr_t)glCreateShaderObjectARB(type));
}

static void c32gl_glLinkProgramARB(C32GLDecoder *d, void *prog) {
    glLinkProgramARB((GLhandleARB)(uintptr_t)prog);
    GLint ok = 1;
    glGetObjectParameterivARB((GLhandleARB)(uintptr_t)prog, GL_OBJECT_LINK_STATUS_ARB, &ok);
    if (!ok && d->errors < 20) {
        char log[2048] = {0};
        glGetInfoLogARB((GLhandleARB)(uintptr_t)prog, sizeof(log) - 1, NULL, log);
        fprintf(stderr, "[C32GL] a shader program failed to link: %s\n", log);
        d->errors++;
        atomic_fetch_add((_Atomic uint32_t *)(d->shm + kOffErrors), 1);
    }
}

/* The stream carries the source as one string. */
static void c32gl_glShaderSourceARB(C32GLDecoder *d, void *shader, const char *src) {
    (void)d;
    if (src) glShaderSourceARB((GLhandleARB)(uintptr_t)shader, 1, &src, NULL);
}

static void c32gl_glPixelStorei(C32GLDecoder *d, uint32_t pname, uint32_t v) {
    (void)d;
    if (pname == GL_UNPACK_CLIENT_STORAGE_APPLE) return; /* the data lives in the stream: GL must copy it */
    glPixelStorei(pname, (GLint)v);
}

/* (Re)creates the game's window framebuffer at w x h. */
static void c32gl_c32WindowSize(C32GLDecoder *d, uint32_t w, uint32_t h) {
    if (w == 0 || h == 0 || ((int)w == d->virtW && (int)h == d->virtH)) return;
    if (!d->virtFBO) {
        glGenFramebuffersEXT(1, &d->virtFBO);
        glGenRenderbuffersEXT(1, &d->virtColor);
        glGenRenderbuffersEXT(1, &d->virtDepth);
        d->drawFBO = d->readFBO = d->virtFBO; /* the game starts drawing into its window */
    }
    GLint rb = 0;
    glGetIntegerv(GL_RENDERBUFFER_BINDING_EXT, &rb);
    glBindRenderbufferEXT(GL_RENDERBUFFER_EXT, d->virtColor);
    /* sRGB-capable, like a Mac window's framebuffer (ToGL writes gamma-encoded colour with
       GL_FRAMEBUFFER_SRGB); presenting copies the encoded values as they are. */
    glRenderbufferStorageEXT(GL_RENDERBUFFER_EXT, GL_SRGB8_ALPHA8_EXT, w, h);
    glBindRenderbufferEXT(GL_RENDERBUFFER_EXT, d->virtDepth);
    glRenderbufferStorageEXT(GL_RENDERBUFFER_EXT, GL_DEPTH24_STENCIL8_EXT, w, h);
    glBindRenderbufferEXT(GL_RENDERBUFFER_EXT, rb);
    glBindFramebufferEXT(GL_FRAMEBUFFER_EXT, d->virtFBO);
    glFramebufferRenderbufferEXT(GL_FRAMEBUFFER_EXT, GL_COLOR_ATTACHMENT0_EXT, GL_RENDERBUFFER_EXT, d->virtColor);
    glFramebufferRenderbufferEXT(GL_FRAMEBUFFER_EXT, GL_DEPTH_ATTACHMENT_EXT, GL_RENDERBUFFER_EXT, d->virtDepth);
    glFramebufferRenderbufferEXT(GL_FRAMEBUFFER_EXT, GL_STENCIL_ATTACHMENT_EXT, GL_RENDERBUFFER_EXT, d->virtDepth);
    GLenum st = glCheckFramebufferStatusEXT(GL_FRAMEBUFFER_EXT);
    if (st != GL_FRAMEBUFFER_COMPLETE_EXT) fprintf(stderr, "[C32GL] the window framebuffer is incomplete (0x%x)\n", st);
    glDrawBuffer(GL_COLOR_ATTACHMENT0_EXT);
    glClearColor(0, 0, 0, 1);
    glClear(GL_COLOR_BUFFER_BIT);
    glBindFramebufferEXT(GL_DRAW_FRAMEBUFFER_EXT, d->drawFBO);
    glBindFramebufferEXT(GL_READ_FRAMEBUFFER_EXT, d->readFBO);
    d->virtW = (int)w;
    d->virtH = (int)h;
    atomic_store(&g_virtW, (int)w);
    atomic_store(&g_virtH, (int)h);
    fprintf(stderr, "[C32GL] game window %ux%u\n", w, h);
}

static void c32gl_c32MouseMode(C32GLDecoder *d, uint32_t relative) {
    (void)d;
    c32SetMouseMode((int)relative);
}

/* A vertex array in the game's memory: a copy for the draw that follows. */
static void c32gl_c32ClientArray(C32GLDecoder *d, uint32_t index, uint32_t size, uint32_t type, uint32_t norm,
                                 uint32_t stride, uint32_t first, uint32_t n, const void *data) {
    uint32_t k = index >= 100 ? 16 + (index - 100) : index;
    if (k >= 104 || !data) return;
    if (d->arraySize[k] < n) {
        free(d->arrays[k]);
        d->arrays[k] = malloc(n);
        d->arraySize[k] = n;
    }
    memcpy(d->arrays[k], data, n);
    uint32_t esize = size * (type == GL_FLOAT || type == GL_INT || type == GL_UNSIGNED_INT ? 4 : type == GL_SHORT || type == GL_UNSIGNED_SHORT ? 2 : 1);
    if (size == GL_BGRA) esize = 4;
    uint32_t step = stride ? stride : esize;
    const uint8_t *ptr = (const uint8_t *)d->arrays[k] - (size_t)first * step; /* so that vertex `first` is at the copy */
    GLint vbo = 0;
    glGetIntegerv(GL_ARRAY_BUFFER_BINDING, &vbo);
    if (vbo) glBindBuffer(GL_ARRAY_BUFFER, 0);
    switch (index) {
    case 100: glVertexPointer((GLint)size, type, (GLsizei)stride, ptr); break;
    case 101: glTexCoordPointer((GLint)size, type, (GLsizei)stride, ptr); break;
    case 102: glColorPointer((GLint)size, type, (GLsizei)stride, ptr); break;
    default: glVertexAttribPointer(index, (GLint)size, type, (GLboolean)norm, (GLsizei)stride, ptr); break;
    }
    if (vbo) glBindBuffer(GL_ARRAY_BUFFER, (GLuint)vbo);
}

static void c32gl_c32GetString(C32GLDecoder *d, uint32_t token, uint32_t name) {
    const char *s = (const char *)glGetString(name);
    reply(d, token, s, s ? (uint32_t)strlen(s) : 0);
}

/* Answer: u32 count, then the values as doubles. */
static void c32gl_c32GetValues(C32GLDecoder *d, uint32_t token, uint32_t pname, uint32_t kind) {
    (void)kind;
    const double sentinel = 1.2345e300;
    double v[16];
    for (int i = 0; i < 16; i++) v[i] = sentinel;
    while (glGetError() != GL_NO_ERROR) {
    }
    glGetDoublev(pname, v);
    if (glGetError() != GL_NO_ERROR) { /* a query this GL doesn't know: no values (not a GL error to report) */
        fprintf(stderr, "[C32GL] the game asked for GL value 0x%x, which this Mac's GL doesn't have\n", pname);
        uint32_t zero = 0;
        reply(d, token, &zero, 4);
        return;
    }
    uint32_t n = 0;
    while (n < 16 && v[n] != sentinel) n++;
    uint8_t out[4 + 16 * 8];
    memcpy(out, &n, 4);
    memcpy(out + 4, v, 8 * n);
    reply(d, token, out, 4 + 8 * n);
}

static void c32gl_c32IsEnabled(C32GLDecoder *d, uint32_t token, uint32_t cap) {
    uint32_t v = glIsEnabled(cap) ? 1 : 0;
    reply(d, token, &v, 4);
}

static void c32gl_c32GetTexLevelParameter(C32GLDecoder *d, uint32_t token, uint32_t target, uint32_t level, uint32_t pname) {
    GLint v = 0;
    glGetTexLevelParameteriv(target, (GLint)level, pname, &v);
    reply(d, token, &v, 4);
}

static uint32_t bytesPerPixel(GLenum format, GLenum type) {
    uint32_t comps = 4;
    switch (format) {
    case GL_RED: case GL_ALPHA: case GL_LUMINANCE: case GL_DEPTH_COMPONENT: case GL_STENCIL_INDEX: comps = 1; break;
    case GL_LUMINANCE_ALPHA: case 0x8227 /* GL_RG */: comps = 2; break;
    case GL_RGB: case GL_BGR: comps = 3; break;
    }
    switch (type) {
    case GL_UNSIGNED_BYTE: case GL_BYTE: return comps;
    case GL_UNSIGNED_SHORT: case GL_SHORT: case 0x140B /* GL_HALF_FLOAT */: return 2 * comps;
    case GL_UNSIGNED_INT: case GL_INT: case GL_FLOAT: return 4 * comps;
    case GL_UNSIGNED_SHORT_5_6_5: case GL_UNSIGNED_SHORT_4_4_4_4: case GL_UNSIGNED_SHORT_4_4_4_4_REV:
    case GL_UNSIGNED_SHORT_1_5_5_5_REV: case GL_UNSIGNED_SHORT_5_5_5_1: return 2;
    }
    return 4;
}

static void c32gl_c32GetTexImage(C32GLDecoder *d, uint32_t token, uint32_t target, uint32_t level, uint32_t format,
                                 uint32_t type, uint32_t compressed) {
    GLint w = 0, h = 0, depth = 1;
    glGetTexLevelParameteriv(target, (GLint)level, GL_TEXTURE_WIDTH, &w);
    glGetTexLevelParameteriv(target, (GLint)level, GL_TEXTURE_HEIGHT, &h);
    glGetTexLevelParameteriv(target, (GLint)level, GL_TEXTURE_DEPTH, &depth);
    if (depth < 1) depth = 1;
    uint32_t n;
    if (compressed) {
        GLint size = 0;
        glGetTexLevelParameteriv(target, (GLint)level, GL_TEXTURE_COMPRESSED_IMAGE_SIZE, &size);
        n = (uint32_t)size;
    } else {
        GLint align = 4, rowLen = 0;
        glGetIntegerv(GL_PACK_ALIGNMENT, &align);
        glGetIntegerv(GL_PACK_ROW_LENGTH, &rowLen);
        uint32_t row = (uint32_t)(rowLen > 0 ? rowLen : w) * bytesPerPixel(format, type);
        if (align > 1) row = (row + (uint32_t)align - 1) / (uint32_t)align * (uint32_t)align;
        n = row * (uint32_t)h * (uint32_t)depth;
    }
    if (n > d->replySize) {
        fprintf(stderr, "[C32GL] a texture readback of %u bytes is too big\n", n);
        reply(d, token, NULL, 0);
        return;
    }
    GLint pbo = 0;
    glGetIntegerv(GL_PIXEL_PACK_BUFFER_BINDING, &pbo);
    if (pbo) glBindBuffer(GL_PIXEL_PACK_BUFFER, 0);
    if (compressed) {
        glGetCompressedTexImage(target, (GLint)level, d->reply);
    } else {
        glGetTexImage(target, (GLint)level, format, type, d->reply);
    }
    if (pbo) glBindBuffer(GL_PIXEL_PACK_BUFFER, (GLuint)pbo);
    reply(d, token, NULL, n);
}

static void c32gl_c32ReadPixels(C32GLDecoder *d, uint32_t token, uint32_t x, uint32_t y, uint32_t w, uint32_t h,
                                uint32_t format, uint32_t type) {
    uint32_t n = w * h * bytesPerPixel(format, type) + 4 * h;
    if (n > d->replySize) {
        reply(d, token, NULL, 0);
        return;
    }
    glReadPixels((GLint)x, (GLint)y, (GLsizei)w, (GLsizei)h, format, type, d->reply);
    reply(d, token, NULL, n);
}

static void c32gl_c32UniformLocation(C32GLDecoder *d, uint32_t token, void *prog, const char *name) {
    GLint loc = name ? glGetUniformLocationARB((GLhandleARB)(uintptr_t)prog, name) : -1;
    reply(d, token, &loc, 4);
}

static void c32gl_c32AttribLocation(C32GLDecoder *d, uint32_t token, void *prog, const char *name) {
    GLint loc = name ? glGetAttribLocationARB((GLhandleARB)(uintptr_t)prog, name) : -1;
    reply(d, token, &loc, 4);
}

static NSOpenGLContext *g_context;
static C32GLView *g_view;
static _Atomic int g_backingW = 1, g_backingH = 1;

/* Shows the game's window, letterboxed, in the view. */
static void c32gl_c32Present(C32GLDecoder *d, uint32_t frame) {
    (void)frame;
    pollQueries(d);
    GLenum err;
    while ((err = glGetError()) != GL_NO_ERROR) {
        if (d->errors < 20) fprintf(stderr, "[C32GL] GL error 0x%x (around %s)\n", err, d->lastCall ? d->lastCall : "?");
        d->errors++;
        atomic_fetch_add((_Atomic uint32_t *)(d->shm + kOffErrors), 1);
    }
    int bw = atomic_load(&g_backingW), bh = atomic_load(&g_backingH);
    if (d->virtFBO && d->virtW > 0 && d->virtH > 0) {
        /* (Saved by hand: glPushAttrib's colour state includes the draw buffer, which would be
           restored onto the window framebuffer.) */
        GLboolean scissor = glIsEnabled(GL_SCISSOR_TEST), srgb = glIsEnabled(GL_FRAMEBUFFER_SRGB_EXT);
        GLfloat clear[4];
        GLboolean mask[4];
        glGetFloatv(GL_COLOR_CLEAR_VALUE, clear);
        glGetBooleanv(GL_COLOR_WRITEMASK, mask);
        glDisable(GL_SCISSOR_TEST);
        glDisable(GL_FRAMEBUFFER_SRGB_EXT);
        glColorMask(GL_TRUE, GL_TRUE, GL_TRUE, GL_TRUE);
        glBindFramebufferEXT(GL_DRAW_FRAMEBUFFER_EXT, 0);
        glDrawBuffer(GL_BACK);
        glClearColor(0, 0, 0, 1);
        glClear(GL_COLOR_BUFFER_BIT);
        double s = (double)bw / d->virtW < (double)bh / d->virtH ? (double)bw / d->virtW : (double)bh / d->virtH;
        int w = (int)(d->virtW * s), h = (int)(d->virtH * s);
        int x = (bw - w) / 2, y = (bh - h) / 2;
        glBindFramebufferEXT(GL_READ_FRAMEBUFFER_EXT, d->virtFBO);
        glReadBuffer(GL_COLOR_ATTACHMENT0_EXT);
        glBlitFramebufferEXT(0, 0, d->virtW, d->virtH, x, y, x + w, y + h, GL_COLOR_BUFFER_BIT, GL_LINEAR);
        if (scissor) glEnable(GL_SCISSOR_TEST);
        if (srgb) glEnable(GL_FRAMEBUFFER_SRGB_EXT);
        glClearColor(clear[0], clear[1], clear[2], clear[3]);
        glColorMask(mask[0], mask[1], mask[2], mask[3]);
    }
    [g_context flushBuffer];
    glBindFramebufferEXT(GL_DRAW_FRAMEBUFFER_EXT, d->drawFBO);
    glBindFramebufferEXT(GL_READ_FRAMEBUFFER_EXT, d->readFBO);
    atomic_fetch_add((_Atomic uint64_t *)(d->shm + kOffFrames), 1);
}

/* ---- Input ---- */

static uint8_t *g_shm;
static _Atomic int g_relative = 0;

static void sendEvent(uint32_t type, uint32_t key, uint32_t mods, int32_t x, int32_t y, int32_t dx, int32_t dy,
                      uint32_t button, const char *text) {
    if (!g_shm) return;
    _Atomic uint32_t *wp = (_Atomic uint32_t *)(g_shm + kOffEventWrite);
    uint32_t w = atomic_load(wp), r = atomic_load((_Atomic uint32_t *)(g_shm + kOffEventRead));
    if (w - r >= kEvents) return; /* the game isn't reading: drop */
    uint8_t *e = g_shm + kEventsOffset + kEventSize * (w % kEvents);
    memset(e, 0, kEventSize);
    uint32_t v[8] = {type, key, mods, (uint32_t)x, (uint32_t)y, (uint32_t)dx, (uint32_t)dy, button};
    memcpy(e, v, sizeof(v));
    if (text) strncpy((char *)e + 32, text, 31);
    atomic_store_explicit(wp, w + 1, memory_order_release);
}

static void c32SetMouseMode(int relative) {
    if (atomic_exchange(&g_relative, relative) == relative) return;
    dispatch_async(dispatch_get_main_queue(), ^{
        CGAssociateMouseAndMouseCursorPosition(relative ? false : true);
        if (relative) {
            [NSCursor hide];
        } else {
            [NSCursor unhide];
        }
    });
}

@implementation C32GLView
- (BOOL)acceptsFirstResponder { return YES; }
- (BOOL)isOpaque { return YES; }

- (void)updateBacking {
    NSRect b = [self convertRectToBacking:[self bounds]];
    atomic_store(&g_backingW, (int)b.size.width > 0 ? (int)b.size.width : 1);
    atomic_store(&g_backingH, (int)b.size.height > 0 ? (int)b.size.height : 1);
    if (g_context) {
        CGLLockContext([g_context CGLContextObj]);
        [g_context update];
        CGLUnlockContext([g_context CGLContextObj]);
    }
}
- (void)setFrameSize:(NSSize)size {
    [super setFrameSize:size];
    [self updateBacking];
}
- (void)viewDidChangeBackingProperties {
    [super viewDidChangeBackingProperties];
    [self updateBacking];
}

/* The game window's coordinates of a point in the view (letterboxed like the picture). */
- (NSPoint)gamePoint:(NSEvent *)event {
    NSPoint p = [self convertPoint:[event locationInWindow] fromView:nil];
    NSRect b = [self bounds];
    int gw = atomic_load(&g_virtW), gh = atomic_load(&g_virtH);
    if (gw <= 0 || gh <= 0 || b.size.width <= 0 || b.size.height <= 0) return p;
    double s = b.size.width / gw < b.size.height / gh ? b.size.width / gw : b.size.height / gh;
    double ox = (b.size.width - gw * s) / 2, oy = (b.size.height - gh * s) / 2;
    double x = (p.x - ox) / s, y = gh - (p.y - oy) / s;
    if (x < 0) x = 0;
    if (y < 0) y = 0;
    if (x > gw - 1) x = gw - 1;
    if (y > gh - 1) y = gh - 1;
    return NSMakePoint(x, y);
}

- (void)keyDown:(NSEvent *)e {
    uint32_t mods = (uint32_t)[e modifierFlags];
    sendEvent(evKeyDown, [e keyCode], mods, 0, 0, 0, 0, [e isARepeat] ? 1 : 0, NULL);
    if (!(mods & (NSEventModifierFlagCommand | NSEventModifierFlagControl))) {
        NSString *chars = [e characters];
        const char *utf8 = [chars UTF8String];
        if (utf8 && utf8[0] && (unsigned char)utf8[0] >= 0x20 && utf8[0] != 0x7f && [chars characterAtIndex:0] < 0xF700) {
            sendEvent(evText, 0, mods, 0, 0, 0, 0, 0, utf8);
        }
    }
}
- (void)keyUp:(NSEvent *)e { sendEvent(evKeyUp, [e keyCode], (uint32_t)[e modifierFlags], 0, 0, 0, 0, 0, NULL); }
- (void)flagsChanged:(NSEvent *)e {
    /* A modifier key: down if its flag is now set. */
    NSEventModifierFlags f = [e modifierFlags], bit = 0;
    switch ([e keyCode]) {
    case 56: case 60: bit = NSEventModifierFlagShift; break;
    case 59: case 62: bit = NSEventModifierFlagControl; break;
    case 58: case 61: bit = NSEventModifierFlagOption; break;
    case 55: case 54: bit = NSEventModifierFlagCommand; break;
    case 57: bit = NSEventModifierFlagCapsLock; break;
    }
    if (bit) sendEvent((f & bit) ? evKeyDown : evKeyUp, [e keyCode], (uint32_t)f, 0, 0, 0, 0, 0, NULL);
}
- (void)motion:(NSEvent *)e {
    NSPoint p = [self gamePoint:e];
    sendEvent(evMotion, 0, (uint32_t)[e modifierFlags], (int32_t)p.x, (int32_t)p.y, (int32_t)[e deltaX], (int32_t)[e deltaY], 0, NULL);
}
- (void)mouseMoved:(NSEvent *)e { [self motion:e]; }
- (void)mouseDragged:(NSEvent *)e { [self motion:e]; }
- (void)rightMouseDragged:(NSEvent *)e { [self motion:e]; }
- (void)otherMouseDragged:(NSEvent *)e { [self motion:e]; }
- (void)button:(NSEvent *)e down:(BOOL)down which:(uint32_t)which {
    NSPoint p = [self gamePoint:e];
    sendEvent(down ? evButtonDown : evButtonUp, 0, (uint32_t)[e modifierFlags], (int32_t)p.x, (int32_t)p.y, 0, 0, which, NULL);
}
- (void)mouseDown:(NSEvent *)e { [self button:e down:YES which:1]; }
- (void)mouseUp:(NSEvent *)e { [self button:e down:NO which:1]; }
- (void)rightMouseDown:(NSEvent *)e { [self button:e down:YES which:3]; }
- (void)rightMouseUp:(NSEvent *)e { [self button:e down:NO which:3]; }
- (void)otherMouseDown:(NSEvent *)e { [self button:e down:YES which:(uint32_t)[e buttonNumber] == 2 ? 2 : (uint32_t)[e buttonNumber] + 2]; }
- (void)otherMouseUp:(NSEvent *)e { [self button:e down:NO which:(uint32_t)[e buttonNumber] == 2 ? 2 : (uint32_t)[e buttonNumber] + 2]; }
- (void)scrollWheel:(NSEvent *)e {
    double dy = [e scrollingDeltaY];
    if ([e hasPreciseScrollingDeltas]) dy /= 10.0;
    int steps = (int)(dy > 0 ? dy + 0.5 : dy - 0.5);
    if (steps) sendEvent(evWheel, 0, 0, 0, 0, 0, steps, 0, NULL);
}
- (void)updateTrackingAreas {
    [super updateTrackingAreas];
    for (NSTrackingArea *a in [self trackingAreas]) [self removeTrackingArea:a];
    [self addTrackingArea:[[NSTrackingArea alloc] initWithRect:[self bounds]
                                                       options:NSTrackingMouseMoved | NSTrackingActiveInKeyWindow | NSTrackingInVisibleRect
                                                         owner:self
                                                      userInfo:nil]];
}
@end

/* ---- The renderer thread ---- */

@implementation C32GLRenderer {
    NSString *_path;
    C32GLView *_view;
    NSThread *_thread;
    _Atomic int _stop;
    uint32_t _screenW, _screenH;
}

- (instancetype)initWithPath:(NSString *)path view:(C32GLView *)view {
    if ((self = [super init])) {
        _path = [path copy];
        _view = view;
        g_view = view;
        NSOpenGLPixelFormatAttribute attrs[] = {
            NSOpenGLPFADoubleBuffer, NSOpenGLPFAAccelerated, NSOpenGLPFAColorSize, 24, NSOpenGLPFAAlphaSize, 8,
            NSOpenGLPFADepthSize, 24, NSOpenGLPFAStencilSize, 8,
            NSOpenGLPFAOpenGLProfile, NSOpenGLProfileVersionLegacy, 0};
        NSOpenGLPixelFormat *pf = [[NSOpenGLPixelFormat alloc] initWithAttributes:attrs];
        g_context = pf ? [[NSOpenGLContext alloc] initWithFormat:pf shareContext:nil] : nil;
        if (!g_context) fprintf(stderr, "[C32GL] no OpenGL context: the game can't draw\n");
        [view setWantsBestResolutionOpenGLSurface:YES];
        [g_context setView:view];
        GLint interval = 1;
        [g_context setValues:&interval forParameter:NSOpenGLContextParameterSwapInterval];
        [view updateBacking];
        NSRect screen = [[NSScreen mainScreen] frame];
        _screenW = (uint32_t)screen.size.width;
        _screenH = (uint32_t)screen.size.height;
    }
    return self;
}

- (void)start {
    if (!g_context || _thread) return;
    [g_context setView:_view]; /* again, now that the window is on screen */
    [_view updateBacking];
    _thread = [[NSThread alloc] initWithTarget:self selector:@selector(run) object:nil];
    [_thread setName:@"C32GL"];
    [_thread setQualityOfService:NSQualityOfServiceUserInteractive];
    [_thread start];
}

- (void)stop {
    atomic_store(&_stop, 1);
    if (atomic_load(&g_relative)) c32SetMouseMode(0);
}

/* Maps the stream once the engine has created it. */
- (uint8_t *)mapStream:(size_t *)size {
    while (!atomic_load(&_stop)) {
        int fd = open([_path fileSystemRepresentation], O_RDWR);
        if (fd >= 0) {
            struct stat st;
            if (fstat(fd, &st) == 0 && st.st_size > (1 << 20)) {
                void *p = mmap(NULL, (size_t)st.st_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
                close(fd);
                if (p != MAP_FAILED) {
                    uint8_t *m = p;
                    for (int i = 0; i < 500 && atomic_load((_Atomic uint32_t *)m) != kMagic; i++) usleep(10000);
                    if (atomic_load((_Atomic uint32_t *)m) == kMagic) {
                        *size = (size_t)st.st_size;
                        return m;
                    }
                    munmap(p, (size_t)st.st_size);
                }
            } else {
                close(fd);
            }
        }
        usleep(50000);
    }
    return NULL;
}

- (void)run {
    size_t size = 0;
    uint8_t *shm = [self mapStream:&size];
    if (!shm) return;
    uint32_t version, ringOff, ringSize, replyOff, replySize;
    memcpy(&version, shm + 4, 4);
    memcpy(&ringOff, shm + 8, 4);
    memcpy(&ringSize, shm + 12, 4);
    memcpy(&replyOff, shm + 16, 4);
    memcpy(&replySize, shm + 20, 4);
    if (version != C32GL_VERSION || (size_t)replyOff + replySize > size || (size_t)ringOff + ringSize > size) {
        fprintf(stderr, "[C32GL] the engine speaks version %u of the GL stream, this window %d: rebuild both\n", version, C32GL_VERSION);
        return;
    }
    C32GLDecoder *d = calloc(1, sizeof(C32GLDecoder));
    d->shm = shm;
    d->ring = shm + ringOff;
    d->ringSize = ringSize;
    d->reply = shm + replyOff;
    d->replySize = replySize;

    CGLContextObj cgl = [g_context CGLContextObj];
    CGLLockContext(cgl);
    [g_context makeCurrentContext];
    int missing = 0;
    for (int i = 0; i < C32GL_NCALLS; i++) {
        if (strncmp(c32glNames[i], "c32", 3) == 0) continue;
        c32glFn[i] = dlsym(RTLD_DEFAULT, c32glNames[i]);
        if (!c32glFn[i] && missing++ < 30) fprintf(stderr, "[C32GL] this Mac's OpenGL has no %s\n", c32glNames[i]);
    }
    fprintf(stderr, "[C32GL] %s, %s\n", glGetString(GL_RENDERER), glGetString(GL_VERSION));
    c32gl_c32WindowSize(d, 640, 480);
    CGLUnlockContext(cgl);

    g_shm = shm;
    atomic_store((_Atomic uint32_t *)(shm + kOffScreenW), _screenW);
    atomic_store((_Atomic uint32_t *)(shm + kOffScreenH), _screenH);
    atomic_store_explicit((_Atomic uint32_t *)(shm + kOffReady), 1, memory_order_release);
    _active = YES;

    _Atomic uint64_t *writeIdx = (_Atomic uint64_t *)(shm + kOffWrite);
    _Atomic uint64_t *readIdx = (_Atomic uint64_t *)(shm + kOffRead);
    uint64_t r = atomic_load(readIdx);
    int idle = 0;
    const char *dbg = getenv("C32_GL_DEBUG"); /* 1: check for GL errors after every call */
    int debug = dbg && dbg[0] == '1';
    while (!atomic_load(&_stop)) {
        uint64_t w = atomic_load_explicit(writeIdx, memory_order_acquire);
        if (r == w) {
            if (atomic_load((_Atomic uint32_t *)(shm + kOffState)) == 2) break; /* the game ended */
            if (d->npending) {
                CGLLockContext(cgl);
                pollQueries(d);
                CGLUnlockContext(cgl);
            }
            usleep(++idle < 200 ? 20 : 1000);
            continue;
        }
        idle = 0;
        CGLLockContext(cgl);
        int n = 0;
        while (r != w && n++ < 4096) {
            uint64_t pos = r % ringSize;
            uint32_t id, len;
            memcpy(&id, d->ring + pos, 4);
            if (id == kSkip) {
                r += ringSize - pos;
            } else {
                memcpy(&len, d->ring + pos + 4, 4);
                if (id < C32GL_NCALLS) d->lastCall = c32glNames[id];
                if (!c32glDecode(d, id, d->ring + pos + 8)) {
                    fprintf(stderr, "[C32GL] unknown call %u: stopping\n", id);
                    atomic_store(&_stop, 1);
                    break;
                }
                if (debug) {
                    GLenum err;
                    while ((err = glGetError()) != GL_NO_ERROR) {
                        if (d->errors++ < 200) {
                            const uint32_t *a = (const uint32_t *)(d->ring + pos + 8);
                            fprintf(stderr, "[C32GL] GL error 0x%x in %s (arguments 0x%x 0x%x 0x%x 0x%x 0x%x 0x%x 0x%x 0x%x 0x%x 0x%x)\n", err, d->lastCall,
                                    len > 0 ? a[0] : 0, len > 4 ? a[1] : 0, len > 8 ? a[2] : 0, len > 12 ? a[3] : 0, len > 16 ? a[4] : 0,
                                    len > 20 ? a[5] : 0, len > 24 ? a[6] : 0, len > 28 ? a[7] : 0, len > 32 ? a[8] : 0, len > 36 ? a[9] : 0);
                        }
                    }
                }
                r += 8 + (uint64_t)len;
            }
            atomic_store_explicit(readIdx, r, memory_order_release);
        }
        CGLUnlockContext(cgl);
    }
    _active = NO;
}

@end
