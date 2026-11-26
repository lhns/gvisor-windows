#include <pthread.h>
#include <sched.h>
#include <stdio.h>
#include <unistd.h>

#define N 8
#define ITER 20000

static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static int started, go;
static long counter;
static __thread int tls;
static __thread char tlsbuf[4096];

static void *worker(void *arg) {
	long id = (long)arg;
	tls = (int)id * 10;
	tlsbuf[4095] = (char)id;
	pthread_mutex_lock(&mu);
	started++;
	pthread_cond_broadcast(&cv);
	while (!go) pthread_cond_wait(&cv, &mu);
	pthread_mutex_unlock(&mu);
	for (int i = 0; i < ITER; i++) {
		pthread_mutex_lock(&mu);
		counter++;
		pthread_mutex_unlock(&mu);
		if (i % 5000 == 0) sched_yield();
	}
	usleep(10000 * (id % 3));
	int ok = tls == (int)id * 10 && tlsbuf[4095] == (char)id;
	if (id % 2) pthread_exit((void *)(long)(ok ? id : -1));
	return (void *)(long)(ok ? id : -1);
}

int main(void) {
	pthread_t t[N];
	for (long i = 0; i < N; i++) pthread_create(&t[i], NULL, worker, (void *)i);
	pthread_mutex_lock(&mu);
	while (started < N) pthread_cond_wait(&cv, &mu);
	go = 1;
	pthread_cond_broadcast(&cv);
	pthread_mutex_unlock(&mu);
	long sum = 0;
	for (int i = 0; i < N; i++) {
		void *r;
		pthread_join(t[i], &r);
		sum += (long)r;
	}
	tls = 1;
	printf("threads: counter=%ld idsum=%ld main_tls=%d\n", counter, sum, tls);
	return counter == (long)N * ITER && sum == 28 ? 0 : 1;
}
