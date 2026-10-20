// glreplay: a stand-in for the window's OpenGL renderer (viewer/C32GL.m) that runs anywhere Mesa
// does: it plays a Source game's GL stream into an offscreen OSMesa context with the window's
// own decoding code, and saves chosen frames as PNG files. For seeing headless runs:
//
//   glreplay <stream file> <out dir> [frames]
//
// frames: a comma-separated list of frame numbers, or every:N (default: every:50). Start it
// before or with the engine (C32_GL_SHM=<stream file> go test ./pkg/source -run TestBoot ...);
// it exits when the game ends. tools/glreplay/build.sh builds it.
#define C32GL_REPLAY 1
#include "../../viewer/C32GL.m"

#include <GL/osmesa.h>
#include <errno.h>

static void c32SetMouseMode(int relative) { (void)relative; }

/* GLREPLAY_CLIP=position: user clip planes test gl_Position instead of gl_ClipVertex, as a
   driver that ignores gl_ClipVertex does (ToGL's renderer table: AMD's Mac drivers). ToGL's
   vertex shaders set gl_ClipVertex before their y and depth fix-ups; this moves it after
   them, onto gl_Position. */
#undef glGetString
/* ...and then it also says it is AMD's, as the window on such a Mac does. */
const GLubyte *glreplayGetString(GLenum name) {
    const char *mode = getenv("GLREPLAY_CLIP");
    if (mode && strcmp(mode, "position") == 0) {
        if (name == GL_VENDOR) return (const GLubyte *)"ATI Technologies Inc.";
        if (name == GL_RENDERER) return (const GLubyte *)"AMD Radeon (simulated on llvmpipe) OpenGL Engine";
    }
    return glGetString(name);
}

#undef glShaderSourceARB
void glreplayShaderSource(GLhandleARB shader, GLsizei count, const GLcharARB **strings, const GLint *lengths) {
    static PFNGLSHADERSOURCEARBPROC real;
    if (!real) real = (PFNGLSHADERSOURCEARBPROC)OSMesaGetProcAddress("glShaderSourceARB");
    const char *mode = getenv("GLREPLAY_CLIP");
    const char *set = "gl_ClipVertex = vTempPos;", *pos = "gl_Position = vTempPos;";
    if (!mode || strcmp(mode, "position") != 0 || count != 1 || !strstr(strings[0], set) || !strstr(strings[0], pos)) {
        real(shader, count, strings, lengths);
        return;
    }
    const char *src = strings[0];
    size_t n = strlen(src);
    char *out = malloc(n + 64), *o = out;
    const char *a = strstr(src, set);
    memcpy(o, src, (size_t)(a - src)), o += a - src; /* drop the early gl_ClipVertex */
    const char *rest = a + strlen(set), *b = strstr(rest, pos) + strlen(pos);
    memcpy(o, rest, (size_t)(b - rest)), o += b - rest;
    o += sprintf(o, " gl_ClipVertex = gl_Position;");
    strcpy(o, b);
    const GLcharARB *one = out;
    real(shader, 1, &one, NULL);
    free(out);
}

static const char *g_out;
static uint64_t g_frame;
static int g_every = 50;
static uint64_t g_frames[256];
static int g_nframes;

static uint32_t crcTable[256];

static uint32_t crc(uint32_t c, const uint8_t *p, size_t n) {
    if (!crcTable[1]) {
        for (uint32_t i = 0; i < 256; i++) {
            uint32_t v = i;
            for (int k = 0; k < 8; k++) v = v & 1 ? 0xEDB88320u ^ (v >> 1) : v >> 1;
            crcTable[i] = v;
        }
    }
    c = ~c;
    while (n--) c = crcTable[(c ^ *p++) & 0xff] ^ (c >> 8);
    return ~c;
}

static void put32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v >> 24), p[1] = (uint8_t)(v >> 16), p[2] = (uint8_t)(v >> 8), p[3] = (uint8_t)v;
}

static void chunk(FILE *f, const char *type, const uint8_t *data, uint32_t n) {
    uint8_t h[8];
    put32(h, n);
    memcpy(h + 4, type, 4);
    fwrite(h, 1, 8, f);
    fwrite(data, 1, n, f);
    uint32_t c = crc(crc(0, (const uint8_t *)type, 4), data, n);
    put32(h, c);
    fwrite(h, 1, 4, f);
}

/* An RGB PNG, uncompressed (stored deflate blocks): rows bottom-up as GL reads them. */
static int writePNG(const char *path, const uint8_t *rgba, int w, int h) {
    FILE *f = fopen(path, "wb");
    if (!f) return -1;
    fwrite("\x89PNG\r\n\x1a\n", 1, 8, f);
    uint8_t ihdr[13];
    put32(ihdr, (uint32_t)w);
    put32(ihdr + 4, (uint32_t)h);
    ihdr[8] = 8, ihdr[9] = 2, ihdr[10] = 0, ihdr[11] = 0, ihdr[12] = 0;
    chunk(f, "IHDR", ihdr, 13);
    size_t row = 1 + 3 * (size_t)w, raw = row * (size_t)h;
    uint8_t *img = malloc(raw);
    for (int y = 0; y < h; y++) {
        uint8_t *o = img + row * (size_t)y;
        const uint8_t *s = rgba + 4 * (size_t)w * (size_t)(h - 1 - y);
        *o++ = 0;
        for (int x = 0; x < w; x++, s += 4) *o++ = s[0], *o++ = s[1], *o++ = s[2];
    }
    size_t blocks = (raw + 65534) / 65535;
    size_t zn = 2 + raw + 5 * blocks + 4;
    uint8_t *z = malloc(zn), *q = z;
    *q++ = 0x78, *q++ = 0x01;
    uint32_t a = 1, b = 0;
    for (size_t i = 0; i < raw; i++) a = (a + img[i]) % 65521, b = (b + a) % 65521;
    for (size_t off = 0; off < raw; off += 65535) {
        uint32_t n = raw - off < 65535 ? (uint32_t)(raw - off) : 65535;
        *q++ = off + n >= raw;
        *q++ = (uint8_t)n, *q++ = (uint8_t)(n >> 8), *q++ = (uint8_t)~n, *q++ = (uint8_t)(~n >> 8);
        memcpy(q, img + off, n);
        q += n;
    }
    put32(q, (b << 16) | a);
    chunk(f, "IDAT", z, (uint32_t)zn);
    chunk(f, "IEND", NULL, 0);
    free(img);
    free(z);
    return fclose(f);
}

static int wanted(uint64_t frame) {
    if (!g_nframes) return g_every > 0 && frame % (uint64_t)g_every == 0;
    for (int i = 0; i < g_nframes; i++) {
        if (g_frames[i] == frame) return 1;
    }
    return 0;
}

/* Present: saves the game's window if this frame is wanted. */
static void c32gl_c32Present(C32GLDecoder *d, uint32_t frame) {
    (void)frame;
    pollQueries(d);
    GLenum err;
    while ((err = glGetError()) != GL_NO_ERROR) {
        if (d->errors < 20) fprintf(stderr, "[glreplay] GL error 0x%x (around %s)\n", err, d->lastCall ? d->lastCall : "?");
        d->errors++;
        atomic_fetch_add((_Atomic uint32_t *)(d->shm + kOffErrors), 1);
    }
    g_frame++;
    if (d->virtFBO && wanted(g_frame)) {
        uint8_t *px = malloc(4 * (size_t)d->virtW * (size_t)d->virtH);
        GLint pack = 0;
        glGetIntegerv(GL_PIXEL_PACK_BUFFER_BINDING, &pack);
        glBindBuffer(GL_PIXEL_PACK_BUFFER, 0);
        glPixelStorei(GL_PACK_ALIGNMENT, 4);
        glBindFramebufferEXT(GL_READ_FRAMEBUFFER_EXT, d->virtFBO);
        glReadBuffer(GL_COLOR_ATTACHMENT0_EXT);
        glReadPixels(0, 0, d->virtW, d->virtH, GL_RGBA, GL_UNSIGNED_BYTE, px);
        glBindBuffer(GL_PIXEL_PACK_BUFFER, (GLuint)pack);
        glBindFramebufferEXT(GL_READ_FRAMEBUFFER_EXT, d->readFBO);
        glReadBuffer(windowBuffer(d, d->readFBO, GL_BACK));
        char path[4096];
        snprintf(path, sizeof path, "%s/frame%05llu.png", g_out, (unsigned long long)g_frame);
        if (writePNG(path, px, d->virtW, d->virtH) == 0) fprintf(stderr, "[glreplay] %s\n", path);
        free(px);
    }
    atomic_fetch_add((_Atomic uint64_t *)(d->shm + kOffFrames), 1);
}

static uint8_t *mapStream(const char *path, size_t *size) {
    for (int tries = 0; tries < 6000; tries++) { /* 5 minutes */
        int fd = open(path, O_RDWR);
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

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: glreplay <stream file> <out dir> [frame,frame,... | every:N]\n");
        return 2;
    }
    g_out = argv[2];
    if (argc > 3) {
        if (strncmp(argv[3], "every:", 6) == 0) {
            g_every = atoi(argv[3] + 6);
        } else {
            for (char *s = argv[3]; *s && g_nframes < 256;) {
                g_frames[g_nframes++] = strtoull(s, &s, 10);
                if (*s == ',') s++;
            }
        }
    }
    OSMesaContext ctx = OSMesaCreateContextExt(OSMESA_RGBA, 24, 8, 0, NULL);
    static uint8_t backbuf[4 * 64 * 64];
    if (!ctx || !OSMesaMakeCurrent(ctx, backbuf, GL_UNSIGNED_BYTE, 64, 64)) {
        fprintf(stderr, "[glreplay] no OSMesa context\n");
        return 1;
    }
    size_t size = 0;
    uint8_t *shm = mapStream(argv[1], &size);
    if (!shm) {
        fprintf(stderr, "[glreplay] no stream at %s\n", argv[1]);
        return 1;
    }
    uint32_t version, ringOff, ringSize, replyOff, replySize;
    memcpy(&version, shm + 4, 4);
    memcpy(&ringOff, shm + 8, 4);
    memcpy(&ringSize, shm + 12, 4);
    memcpy(&replyOff, shm + 16, 4);
    memcpy(&replySize, shm + 20, 4);
    if (version != C32GL_VERSION || (size_t)replyOff + replySize > size || (size_t)ringOff + ringSize > size) {
        fprintf(stderr, "[glreplay] the engine speaks version %u of the GL stream, glreplay %d: rebuild both\n", version, C32GL_VERSION);
        return 1;
    }
    C32GLDecoder *d = calloc(1, sizeof(C32GLDecoder));
    d->shm = shm;
    d->ring = shm + ringOff;
    d->ringSize = ringSize;
    d->reply = shm + replyOff;
    d->replySize = replySize;
    for (int i = 0; i < C32GL_NCALLS; i++) {
        if (strncmp(c32glNames[i], "c32", 3) == 0) continue;
        c32glFn[i] = (void *)OSMesaGetProcAddress(c32glNames[i]);
    }
    fprintf(stderr, "[glreplay] %s, %s\n", glGetString(GL_RENDERER), glGetString(GL_VERSION));
    c32gl_c32WindowSize(d, 640, 480);
    unsigned sw = 1440, sh = 900; /* the screen the game sees: GLREPLAY_SCREEN=WxH */
    if (getenv("GLREPLAY_SCREEN")) sscanf(getenv("GLREPLAY_SCREEN"), "%ux%u", &sw, &sh);
    atomic_store((_Atomic uint32_t *)(shm + kOffScreenW), sw);
    atomic_store((_Atomic uint32_t *)(shm + kOffScreenH), sh);
    atomic_store_explicit((_Atomic uint32_t *)(shm + kOffReady), 1, memory_order_release);

    _Atomic uint64_t *writeIdx = (_Atomic uint64_t *)(shm + kOffWrite);
    _Atomic uint64_t *readIdx = (_Atomic uint64_t *)(shm + kOffRead);
    uint64_t r = atomic_load(readIdx);
    const char *dbg = getenv("C32_GL_DEBUG"); /* 1: check for GL errors after every call */
    int debug = dbg && dbg[0] == '1';
    for (int idle = 0;;) {
        uint64_t w = atomic_load_explicit(writeIdx, memory_order_acquire);
        if (r == w) {
            if (atomic_load((_Atomic uint32_t *)(shm + kOffState)) == 2) break; /* the game ended */
            if (d->npending) pollQueries(d);
            usleep(++idle < 200 ? 20 : 1000);
            continue;
        }
        idle = 0;
        while (r != w) {
            uint64_t pos = r % ringSize;
            uint32_t id, len;
            memcpy(&id, d->ring + pos, 4);
            if (id == kSkip) {
                r += ringSize - pos;
            } else {
                memcpy(&len, d->ring + pos + 4, 4);
                if (id < C32GL_NCALLS) d->lastCall = c32glNames[id];
                if (!c32glDecode(d, id, d->ring + pos + 8)) {
                    fprintf(stderr, "[glreplay] unknown call %u: stopping\n", id);
                    return 1;
                }
                if (debug) {
                    GLenum err;
                    while ((err = glGetError()) != GL_NO_ERROR) {
                        if (d->errors++ < 200) fprintf(stderr, "[glreplay] GL error 0x%x in %s\n", err, d->lastCall);
                    }
                }
                r += 8 + (uint64_t)len;
            }
            atomic_store_explicit(readIdx, r, memory_order_release);
        }
    }
    fprintf(stderr, "[glreplay] %llu frames, %u GL errors\n", (unsigned long long)g_frame, d->errors);
    return 0;
}
