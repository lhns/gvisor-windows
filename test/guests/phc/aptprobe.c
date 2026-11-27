// aptrename with heap probes (probe.h) around every DIR.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include "probe.h"
static DIR *p_opendir(const char *p) { return probe_opendir(p); }
static struct dirent *p_readdir(DIR *d) { return probe_readdir(d); }
static int p_closedir(DIR *d) { return probe_closedir(d); }
#define opendir p_opendir
#define readdir p_readdir
#define closedir p_closedir
#undef _GNU_SOURCE
#include "../aptrename.c"
