package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/AlienResidents/skbn/pkg/skbn"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/deprecated/scheme"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
)

func (i *pod) copyFromPod(srcPath string, destPath string) error {
	restconfig, err, coreclient := InitRestClient()
	reader, outStream := io.Pipe()
	//todo some containers failed : tar: Refusing to write archive contents to terminal (missing -f option?) when execute `tar cf -` in container
	cmdArr := []string{"tar", "cf", "-", srcPath}
	req := coreclient.RESTClient().
		Get().
		Namespace(i.Namespace).
		Resource("pods").
		Name(i.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: i.ContainerName,
			Command:   cmdArr,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restconfig, "POST", req.URL())
	if err != nil {
		log.Fatalf("error %s\n", err)
		return err
	}
	go func() {
		defer outStream.Close()
		err = exec.Stream(remotecommand.StreamOptions{
			Stdin:  os.Stdin,
			Stdout: outStream,
			Stderr: os.Stderr,
			Tty:    false,
		})
		cmdutil.CheckErr(err)
	}()
	prefix := getPrefix(srcPath)
	prefix = path.Clean(prefix)
	prefix = cpStripPathShortcuts(prefix)
	destPath = path.Join(destPath, path.Base(prefix))
	err = untarAll(reader, destPath, prefix)
	return err
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println(err.Error())
		log.Fatal(err.Error())
	}

	checkVaultNamespace := "tm-system"
	checkVaultDeployment := "check-vault"
	var checkVaultPod v1.Pod
	checkVaultContainer := "check-vault"
	var checkVaultDstPod v1.Pod
	checkVaultDstContainer := "check-vault-omatic"

	// Regexes searches
	s1 := "check-vault-"
	s2 := "check-vault-omatic-"

	// Compiled regexes
	r1 := regexp.MustCompile(s1)
	r2 := regexp.MustCompile(s2)

	log.Printf("Starting check-vault-omatic\n")

	// Start the HTTP file server
	http.HandleFunc("/check-vault", func(w http.ResponseWriter, r *http.Request) {
		remoteIP := r.Header.Get("x-forwarded-for")
		if r.Header.Get("x-forwarded-for") == "" {
			remoteIP = r.RemoteAddr
		}
		log.Printf("Connection from %s\n", remoteIP)
		pods, err := clientset.CoreV1().Pods(checkVaultNamespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Println(w, err.Error())
			log.Println(err.Error())
		}

		//deployment, err := clientset.AppsV1().Deployments(checkVaultNamespace).Get(context.TODO(), checkVaultDeployment, metav1.GetOptions{})
		deploymentClient := clientset.ExtensionsV1beta1().Deployments(checkVaultNamespace)
		if err != nil {
			fmt.Println(w, err.Error())
			log.Println(err.Error())
		}

		deployment, err := deploymentClient.Get(context.TODO(), checkVaultDeployment, metav1.GetOptions{})
		if err != nil {
			fmt.Println(w, err.Error())
			log.Println(err.Error())
		} else {
			log.Printf("Found deployment: %s \n", deployment.Name)
		}

		// TODO: fix assumption that replicas is 0 already
		// Set replicas to 1
		log.Printf("Patching deployment replicas to 1 for %s \n", deployment.Name)
		patch := []byte(`{"spec":{"replicas":1}}`)
		_, err = deploymentClient.Patch(context.TODO(), checkVaultDeployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: remoteIP})
		if err != nil {
			log.Printf("Replicas could not be set\n")
			log.Println(err.Error())
		} else {
			log.Printf("Replicas set to 1 successfully\n")
		}

		log.Println("Searching for the check-vault, and check-vault-omatic PODs")
		for _, pod := range pods.Items {
			log.Printf("Found a POD: %s\n", pod.Name)
			if r1.Match([]byte(pod.Name)) && !r2.Match([]byte(pod.Name)) {
				log.Printf("Found the check-vault POD: %s\n", pod.Name)
				checkVaultPod = pod
			} else if r2.Match([]byte(pod.Name)) {
				log.Printf("Found the check-vault-omatic POD: %s\n", pod.Name)
				checkVaultDstPod = pod
			}
		}

		for container := range checkVaultPod.Spec.Containers {
			log.Printf("Found a container in the %s POD: %s: %s\n", checkVaultPod.Name, checkVaultContainer, checkVaultPod.Spec.Containers[container].Name)
		}

		now := time.Now()
		RemoteCheckVaultFilePath := "/home/app/check-vault.zip"
		CheckVaultZipDir := homedir.HomeDir()
		CheckVaultZipFile := "check-vault-" + now.Format("20060102-150405") + ".zip"
		FullPathCheckVaultZipFile := filepath.Join(CheckVaultZipDir, CheckVaultZipFile)

		time.Sleep(1 * time.Minute)

		// Copy the file from the container check-vault, in pod check-vault to CheckVaultZipDir
		src := "k8s://" + checkVaultNamespace + "/" + checkVaultPod.Name + "/" + checkVaultContainer + RemoteCheckVaultFilePath
		dst := "k8s://" + checkVaultNamespace + "/" + checkVaultDstPod.Name + "/" + checkVaultDstContainer + FullPathCheckVaultZipFile
		parallel := 0
		bufferSize := 0.05
		if copyErr := skbn.Copy(src, dst, parallel, bufferSize); copyErr != nil {
			log.Printf("Failed to transfer %s in namespace %s to %s POD in %s container.\n", FullPathCheckVaultZipFile, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
			log.Println(copyErr.Error())
		} else {
			fileStat, err := os.Stat(FullPathCheckVaultZipFile)
			if err != nil {
				log.Printf("Failed to transfer %s in namespace %s to %s POD in %s container.\n", RemoteCheckVaultFilePath, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
				log.Println(err.Error())
			} else {
				log.Printf("Successfully transferred %s in namespace %s to %s POD in %s container.\n", RemoteCheckVaultFilePath, checkVaultNamespace, checkVaultDstPod.Name, checkVaultDstContainer)
				log.Println(fileStat)
				log.Printf("Sending %s to %s\n", FullPathCheckVaultZipFile, remoteIP)
				w.Header().Set("Content-Disposition", "attachment; filename="+CheckVaultZipFile)
				http.ServeFile(w, r, FullPathCheckVaultZipFile)
			}
		}

		//log.Printf("Patching deployment replicas to 0 for %s \n", deployment.Name)
		//patch = []byte(`{"spec":{"replicas":0}}`)
		//_, err = deploymentClient.Patch(context.TODO(), checkVaultDeployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: remoteIP})
		//if err != nil {
		//	log.Printf("Replicas could not be set\n")
		//	log.Println(err.Error())
		//} else {
		//	log.Printf("Replicas set 0 successfully\n")
		//}

	})

	log.Fatal(http.ListenAndServe(":8078", nil))
}
