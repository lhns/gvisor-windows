#include <dlfcn.h>
#include <stdio.h>

int main(void) {
	void *h = dlopen("/lib/libplug.so", RTLD_NOW);
	if (!h) { printf("dlopen failed: %s\n", dlerror()); return 1; }
	int (*add)(int, int) = dlsym(h, "plug_add");
	int *(*tls)(void) = dlsym(h, "plug_tls");
	void *m = dlopen("libm.so.6", RTLD_NOW);
	if (!add || !tls || !m) { printf("dlsym failed: %s\n", dlerror()); return 2; }
	double (*c)(double) = dlsym(m, "cos");
	int r = add(1, 2);
	*tls() = 7;
	printf("dlopen ok: plug=%d tls=%d cos=%.1f\n", r, add(0, 0), c(0.0));
	fflush(stdout);
	dlclose(h);
	return 0;
}
