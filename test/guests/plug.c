/* Shared library for the dlopen guest: a constructor and a TLS variable. */
__thread int ptv = 42;
static int ctor_ran;
__attribute__((constructor)) static void init(void) { ctor_ran = 1; }
int plug_add(int a, int b) { return a + b + ptv + ctor_ran; }
int *plug_tls(void) { return &ptv; }
