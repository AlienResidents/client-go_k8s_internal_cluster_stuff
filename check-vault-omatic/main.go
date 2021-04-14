package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AlienResidents/skbn/pkg/skbn"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/homedir"
)

func main() {
	var mu sync.Mutex

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err.Error())
	}

	log.Printf("Starting check-vault-omatic")

	// Start the HTTP file server
	http.HandleFunc("/check-vault", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		checkVaultNamespace := "tm-system"
		checkVaultDeployment := "check-vault"
		var checkVaultPod apiv1.Pod
		checkVaultContainer := "check-vault"
		var checkVaultDstPod apiv1.Pod
		checkVaultDstContainer := "check-vault-omatic"

		thisHost := r.Host
		splitHost := strings.Split(thisHost, ".")
		thisEnv := splitHost[2]
		remoteIP := r.Header.Get("x-forwarded-for")
		if r.Header.Get("x-forwarded-for") == "" {
			remoteIP = r.RemoteAddr
		}
		log.Printf("Connection from %s", remoteIP)

		deploymentClient := clientset.AppsV1().Deployments(checkVaultNamespace)
		if err != nil {
			log.Println(err.Error())
		}

		deployment, err := deploymentClient.Get(context.TODO(), checkVaultDeployment, metav1.GetOptions{})
		if err != nil {
			log.Println(err.Error())
		} else {
			log.Printf("Found deployment: %s", deployment.Name)
		}

		// TODO: Ensure that spec.replicas is 0
		// Set replicas to 1
		log.Printf("Patching deployment replicas to 1 for %s", deployment.Name)
		patch := []byte(`{"spec":{"replicas":1}}`)
		_, err = deploymentClient.Patch(context.TODO(), checkVaultDeployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: remoteIP})
		if err != nil {
			log.Printf("Replicas could not be set")
			log.Println(err.Error())
		} else {
			log.Printf("Replicas set to 1 successfully")
		}

		// TODO implement watch until for POD status to be Ready, and that the check-vault.zip file has been generated (another pod/exec call)
		// https://godoc.org/k8s.io/client-go/tools/watch
		log.Printf("Sleeping for 1 minute")
		time.Sleep(1 * time.Minute)

		pods, err := clientset.CoreV1().Pods(checkVaultNamespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Println(err.Error())
		}

		// Regex searches
		s1 := "check-vault-"
		s2 := "check-vault-omatic-"

		// Compiled regexes
		r1 := regexp.MustCompile(s1)
		r2 := regexp.MustCompile(s2)

		log.Println("Searching for the check-vault, and check-vault-omatic PODs")
		for _, pod := range pods.Items {
			log.Printf("Found a POD: %s", pod.Name)
			if r1.Match([]byte(pod.Name)) && !r2.Match([]byte(pod.Name)) {
				log.Printf("Found the check-vault POD: %s", pod.Name)
				checkVaultPod = pod
			} else if r2.Match([]byte(pod.Name)) {
				log.Printf("Found the check-vault-omatic POD: %s", pod.Name)
				checkVaultDstPod = pod
			}
		}

		if checkVaultPod.Name != "" {
			for container := range checkVaultPod.Spec.Containers {
				log.Printf("Found a container in the %s POD: %s: %s", checkVaultPod.Name, checkVaultContainer, checkVaultPod.Spec.Containers[container].Name)
			}
		} else {
			log.Fatal("Did NOT find a check-vault POD")
		}

		now := time.Now()
		RemoteCheckVaultFilePath := "/home/tm-app/check-vault.zip"
		CheckVaultZipDir := homedir.HomeDir()
		CheckVaultZipFile := "check-vault-" + thisEnv + "-" + now.Format("20060102-150405") + ".zip"
		FullPathCheckVaultZipFile := filepath.Join(CheckVaultZipDir, CheckVaultZipFile)

		// Copy the file from the container check-vault, in pod check-vault to CheckVaultZipDir
		src := "k8s://" + checkVaultNamespace + "/" + checkVaultPod.Name + "/" + checkVaultContainer + RemoteCheckVaultFilePath
		dst := "k8s://" + checkVaultNamespace + "/" + checkVaultDstPod.Name + "/" + checkVaultDstContainer + FullPathCheckVaultZipFile
		parallel := 0
		bufferSize := 0.05
		if err := skbn.Copy(src, dst, parallel, bufferSize); err != nil {
			log.Printf("Failed to transfer %s in namespace %s to %s POD in %s container.", FullPathCheckVaultZipFile, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
			log.Println(err.Error())
		} else {
			fileStat, err := os.Stat(FullPathCheckVaultZipFile)
			if err != nil {
				log.Printf("Failed to transfer %s in namespace %s to %s POD in %s container.", RemoteCheckVaultFilePath, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
				log.Println(err.Error())
			} else {
				log.Printf("Successfully transferred %s in namespace %s to %s POD in %s container.", RemoteCheckVaultFilePath, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
				log.Println(fileStat)
				log.Printf("Sending %s to %s", FullPathCheckVaultZipFile, remoteIP)
				w.Header().Set("Content-Disposition", "attachment; filename="+CheckVaultZipFile)
				http.ServeFile(w, r, FullPathCheckVaultZipFile)
			}
		}

		log.Printf("Patching deployment replicas to 0 for %s", deployment.Name)
		patch = []byte(`{"spec":{"replicas":0}}`)
		_, err = deploymentClient.Patch(context.TODO(), checkVaultDeployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: remoteIP})
		if err != nil {
			log.Printf("Replicas could not be set")
			log.Println(err.Error())
		} else {
			log.Printf("Replicas set 0 successfully")
		}

	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
