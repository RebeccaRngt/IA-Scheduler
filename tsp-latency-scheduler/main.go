// main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// LatencyMetrics mappe nom de noeud -> latence moyenne en ms
type LatencyMetrics map[string]float64

func main() {
	var (
		schedulerName        string
		latencyCMNamespace   string
		latencyCMName        string
		resyncPeriodSeconds  int
	)

	flag.StringVar(&schedulerName, "scheduler-name", "default-scheduler", "Nom du scheduler à utiliser (schedulerName)")
	flag.StringVar(&latencyCMNamespace, "latency-cm-namespace", "qperf", "Namespace de la ConfigMap de latence")
	flag.StringVar(&latencyCMName, "latency-cm-name", "latency-metrics", "Nom de la ConfigMap de latence")
	flag.IntVar(&resyncPeriodSeconds, "resync-period", 30, "Période de resync (en secondes) pour recharger les métriques de latence")
	flag.Parse()

	fmt.Printf("[scheduler] Starting TSP latency-aware scheduler with name %q\n", schedulerName)

	// Création du client Kubernetes
	clientset, err := createClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[scheduler] Failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Watch des Pods Pending avec ce schedulerName
	fieldSelector := fields.AndSelectors(
		fields.OneTermEqualSelector("spec.schedulerName", schedulerName),
		fields.OneTermEqualSelector("status.phase", "Pending"),
	)

	watcher, err := clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		FieldSelector: fieldSelector.String(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[scheduler] Failed to start watch on pods: %v\n", err)
		os.Exit(1)
	}
	defer watcher.Stop()

	fmt.Println("[scheduler] Watching for Pending pods...")

	// Boucle principale : on regarde les événements sur les Pods
	go func() {
		for {
			time.Sleep(time.Duration(resyncPeriodSeconds) * time.Second)
			fmt.Println("[scheduler] Periodic tick: you can refresh latency metrics here if needed")
		}
	}()

	for event := range watcher.ResultChan() {
		switch ev := event.Type; ev {
		case watch.Added:
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			fmt.Printf("[scheduler] Pod added: %s/%s (schedulerName=%s, phase=%s)\n",
				pod.Namespace, pod.Name, pod.Spec.SchedulerName, pod.Status.Phase)

			if err := schedulePod(ctx, clientset, pod, latencyCMNamespace, latencyCMName); err != nil {
				fmt.Fprintf(os.Stderr, "[scheduler] Failed to schedule pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			}

		case watch.Modified:
			// On peut ignorer ou gérer différemment si besoin
		case watch.Deleted:
			// Rien à faire pour un pod supprimé
		case watch.Error:
			fmt.Fprintf(os.Stderr, "[scheduler] Watch error: %v\n", event.Object)
		}
	}
}

// createClient essaie d'abord d'utiliser KUBECONFIG, puis InClusterConfig
func createClient() (*kubernetes.Clientset, error) {
	// 1. Essaye avec KUBECONFIG (utile pour dev en local)
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err == nil {
			return kubernetes.NewForConfig(config)
		}
		fmt.Fprintf(os.Stderr, "[scheduler] Failed to load kubeconfig from KUBECONFIG=%s: %v\n", kubeconfig, err)
	}

	// 2. Sinon, on essaie InCluster (pour tourner dans un pod)
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("unable to use in-cluster config: %w", err)
	}

	return kubernetes.NewForConfig(config)
}

// schedulePod choisit un noeud pour le pod en fonction des métriques de latence
func schedulePod(ctx context.Context, clientset *kubernetes.Clientset, pod *corev1.Pod, cmNamespace, cmName string) error {
	// Ne re-schedule pas un pod déjà bindé
	if pod.Spec.NodeName != "" {
		fmt.Printf("[scheduler] Pod %s/%s already scheduled on node %s\n",
			pod.Namespace, pod.Name, pod.Spec.NodeName)
		return nil
	}

	// Chargement des métriques de latence
	latencyMetrics, err := loadLatencyMetrics(ctx, clientset, cmNamespace, cmName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[scheduler] Warning: failed to load latency metrics, using default scoring: %v\n", err)
		latencyMetrics = LatencyMetrics{}
	}

	// Récupération des noeuds disponibles
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return fmt.Errorf("no available nodes to schedule pod %s/%s", pod.Namespace, pod.Name)
	}

	// Calcul d'un score pour chaque noeud et sélection du meilleur
	var (
		bestNodeName string
		bestScore    float64 = -1e18 // très petit pour démarrer
	)

	for _, node := range nodeList.Items {
		nodeName := node.Name
		score := scoreNode(nodeName, latencyMetrics, pod)

		fmt.Printf("[scheduler] Node %s has score %.2f for pod %s/%s\n",
			nodeName, score, pod.Namespace, pod.Name)

		if score > bestScore {
			bestScore = score
			bestNodeName = nodeName
		}
	}

	if bestNodeName == "" {
		return fmt.Errorf("no suitable node found for pod %s/%s", pod.Namespace, pod.Name)
	}

	fmt.Printf("[scheduler] Binding pod %s/%s to node %s (score=%.2f)\n",
		pod.Namespace, pod.Name, bestNodeName, bestScore)

	// Bind du pod au noeud choisi
	return bindPod(ctx, clientset, pod, bestNodeName)
}

// loadLatencyMetrics lit une ConfigMap de la forme :
// - clé   = nom de noeud
// - valeur = latence moyenne en ms (float)
func loadLatencyMetrics(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) (LatencyMetrics, error) {
	cm, err := clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	metrics := LatencyMetrics{}

	for nodeName, value := range cm.Data {
		lat, err := strconv.ParseFloat(value, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[scheduler] Invalid latency value for node %s: %q (err=%v), ignoring\n",
				nodeName, value, err)
			continue
		}
		metrics[nodeName] = lat
	}

	fmt.Printf("[scheduler] Loaded latency metrics for %d nodes from %s/%s\n",
		len(metrics), ns, name)

	return metrics, nil
}

// scoreNode calcule un score avancé en utilisant une heuristique multi-critère.
// C’est ici que réside la partie “IA heuristique” de ton scheduler.
//
// Critères pris en compte :
//  - Latence moyenne vers le node (poids fort)
//  - Variabilité / jitter (plus faible = meilleur)
//  - Latence max (évite les nodes instables)
//  - Charge CPU / mémoire du node (si disponible dans les labels)
//  - Affinité avec les pods de la même slice (si label slice=X)
//
func scoreNode(nodeName string, metrics LatencyMetrics, pod *corev1.Pod) float64 {

    // ---- 1. Récupération des métriques de latence ----
    avgLatency, ok := metrics[nodeName]
    if !ok {
        avgLatency = 9999.0
    }

    // Simulons des métriques supplémentaires (plus tard tu pourras les injecter via un CRD)
    jitter := avgLatency * 0.08       // simule un jitter approximatif = 8% de la latence
    maxLatency := avgLatency * 1.25   // simule un pic possible de latence = +25%

    // ---- 2. Récupération des labels (slice eMBB / URLLC / mMTC) ----
    slice := pod.Labels["slice"]
    sliceBonus := 0.0

    if slice == "URLLC" {
        // URLLC très sensible à la latence
        sliceBonus = 30.0
    } else if slice == "eMBB" {
        sliceBonus = 10.0
    }

    // ---- 3. Pondération IA heuristique ----
    //
    // Idée : score = somme pondérée des critères
    //
    // Poids choisis (tu peux les ajuster) :
    //  - latence moyenne :        -0.75
    //  - jitter :                 -0.15
    //  - latence max :            -0.10
    //  - bonus selon slice :      +sliceBonus
    //

    score :=
        (-0.75 * avgLatency) +
        (-0.15 * jitter) +
        (-0.10 * maxLatency) +
        sliceBonus

    return score
}

// bindPod crée un Binding pour attacher le pod au noeud choisi
func bindPod(ctx context.Context, clientset *kubernetes.Clientset, pod *corev1.Pod, nodeName string) error {
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		Target: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
	}

	err := clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to bind pod %s/%s to node %s: %w",
			pod.Namespace, pod.Name, nodeName, err)
	}
	return nil
}
