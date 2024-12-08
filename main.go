package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-redis/redis/v7"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	config := ctrl.GetConfigOrDie()

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	userToObjectPatternMapping, groupToObjectPatternMapping := loadRBACPolicyFromConfigMap(clientset, "argocd", "argocd-rbac-cm")

	redisClient := initializeRedis("localhost:16379", "", 1)

	proxy := createReverseProxy("http://localhost:8080")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleRequest(w, r, proxy, redisClient, userToObjectPatternMapping, groupToObjectPatternMapping)
	})

	log.Println("Proxy server running on :8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}

func loadRBACPolicyFromConfigMap(clientset *kubernetes.Clientset, namespace, configMapName string) (map[string][]string, map[string][]string) {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), configMapName, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Failed to fetch ConfigMap %s: %v", configMapName, err)
		return nil, nil
	}

	policyCSV, ok := cm.Data["policy.csv"]
	if !ok {
		fmt.Printf("policy.csv not found in ConfigMap %s\n", configMapName)
		return nil, nil
	}

	return parsePolicyCSV(policyCSV)
}

func initializeRedis(addr, password string, db int) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DB:          db,
		DialTimeout: 5 * time.Second,
	})

	if _, err := client.Ping().Result(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	fmt.Println("Connected to Redis successfully")
	return client
}

func createReverseProxy(target string) *httputil.ReverseProxy {
	parsedURL, err := url.Parse(target)
	if err != nil {
		log.Fatalf("Invalid ArgoCD server URL: %v", err)
	}

	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = parsedURL.Scheme
			req.URL.Host = parsedURL.Host
			req.Host = parsedURL.Host
		},
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request, proxy *httputil.ReverseProxy, redisClient *redis.Client, userToObjectPatternMapping, groupToObjectPatternMapping map[string][]string) {
	token := extractToken(r)
	if token == "" || (r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/applications")) {
		proxy.ServeHTTP(w, r)
		return
	}

	payload, err := decodeJWTPayload(token)
	if err != nil {
		proxy.ServeHTTP(w, r)
		return
	}

	email, _ := payload["email"].(string)
	groups, _ := payload["groups"].([]string)

	objectPatterns := resolveObjectPatterns(email, groups, userToObjectPatternMapping, groupToObjectPatternMapping)

	resp := fetchApplicationsFromRedis(redisClient, objectPatterns)
	if len(resp.Items) == 0 {
		proxy.ServeHTTP(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to write response: %v", err)
		proxy.ServeHTTP(w, r)
	}
}

func extractToken(r *http.Request) string {
	if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if cookie, err := r.Cookie("argocd.token"); err == nil {
		return cookie.Value
	}
	return ""
}

func decodeJWTPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid token format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode payload: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %v", err)
	}
	return payload, nil
}

func resolveObjectPatterns(email string, groups []string, userToObjectPatternMapping, groupToObjectPatternMapping map[string][]string) map[string]struct{} {
	objectPatterns := make(map[string]struct{})

	for _, pattern := range userToObjectPatternMapping[email] {
		objectPatterns[pattern] = struct{}{}
	}

	for _, group := range groups {
		for _, pattern := range groupToObjectPatternMapping[group] {
			objectPatterns[pattern] = struct{}{}
		}
	}

	return objectPatterns
}

func fetchApplicationsFromRedis(redisClient *redis.Client, objectPatterns map[string]struct{}) struct {
	Items []interface{} `json:"items"`
} {
	resp := struct {
		Items []interface{} `json:"items"`
	}{Items: []interface{}{}}

	var allKeys []string
	for pattern := range objectPatterns {
		keys, err := redisClient.Keys(fmt.Sprintf("%s|*", pattern)).Result()
		if err != nil {
			log.Printf("Failed to fetch keys for pattern %s: %v", pattern, err)
			continue
		}
		allKeys = append(allKeys, keys...)
	}

	if len(allKeys) > 0 {
		pipe := redisClient.Pipeline()
		cmds := make([]*redis.StringCmd, len(allKeys))
		for i, key := range allKeys {
			cmds[i] = pipe.Get(key)
		}
		_, err := pipe.Exec()
		if err != nil && err != redis.Nil {
			log.Printf("Failed to fetch values for keys: %v", err)
		}

		for i, cmd := range cmds {
			if cmd.Err() == nil {
				var rawJson interface{}
				if err := json.Unmarshal([]byte(cmd.Val()), &rawJson); err == nil {
					resp.Items = append(resp.Items, rawJson)
				} else {
					log.Printf("Failed to unmarshal value for key %s: %v", allKeys[i], err)
				}
			} else {
				log.Printf("Failed to fetch value for key %s: %v", allKeys[i], cmd.Err())
			}
		}
	}
	return resp
}

func parsePolicyCSV(policyCSV string) (map[string][]string, map[string][]string) {
	userToRoleMapping := make(map[string][]string)
	groupToRoleMapping := make(map[string][]string)

	roleToObjectPatternMapping := make(map[string][]string)
	// default rules
	// - role:admin: unrestricted access to all objects
	// - role:readonly: read-only access to all objects
	roleToObjectPatternMapping["role:admin"] = []string{"*"}
	roleToObjectPatternMapping["role:readonly"] = []string{"*"}

	lines := strings.Split(policyCSV, "\n")
	for _, line := range lines {
		// Ignore empty lines and comments
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split the line into fields
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i]) // Trim spaces around each field
		}

		// Process "g" entries (group-role mappings)
		if fields[0] == "g" && len(fields) >= 3 {
			userOrGroup := fields[1]
			role := fields[2]

			if strings.Contains(userOrGroup, "@") {
				// Process user-role mappings
				user := userOrGroup
				if _, exists := userToRoleMapping[user]; !exists {
					// Initialize the role in the groupToRoleMapping map if it doesn't exist
					userToRoleMapping[user] = []string{}
				}
				userToRoleMapping[user] = append(userToRoleMapping[user], role)
			} else {
				// Process group-role mappings
				group := userOrGroup
				if _, exists := groupToRoleMapping[group]; !exists {
					// Initialize the role in the groupToRoleMapping map if it doesn't exist
					groupToRoleMapping[group] = []string{}
				}
				groupToRoleMapping[group] = append(groupToRoleMapping[group], role)
			}
		}

		// Process "p" entries (role-resource mappings)
		if fields[0] == "p" && len(fields) >= 5 {
			role := fields[1]
			resource := fields[2]
			// action := fields[3]
			objectPattern := fields[4]
			// effect := field[5]

			if resource == "applications" || resource == "applicationsets" || resource == "logs" || resource == "exec " {
				objectPattern = strings.TrimSuffix(objectPattern, "/*")
			}

			if _, exists := roleToObjectPatternMapping[role]; !exists {
				// Initialize the role in the roleToObjectPatternMapping map if it doesn't exist
				roleToObjectPatternMapping[role] = []string{}
			}
			roleToObjectPatternMapping[role] = append(roleToObjectPatternMapping[role], objectPattern)
		}
	}

	// Aggregate the user to object pattern mapping
	userToObjectPatternMapping := make(map[string][]string)
	for user, roles := range userToRoleMapping {
		userToObjectPatternMapping[user] = []string{}

		for _, role := range roles {
			if objectPatterns, exists := roleToObjectPatternMapping[role]; exists {
				userToObjectPatternMapping[user] = append(userToObjectPatternMapping[user], objectPatterns...)
			}
		}
	}

	// Aggregate the group to object pattern mapping
	groupToObjectPatternMapping := make(map[string][]string)
	for group, roles := range groupToRoleMapping {
		groupToObjectPatternMapping[group] = []string{}

		for _, role := range roles {
			if objectPatterns, exists := roleToObjectPatternMapping[role]; exists {
				groupToObjectPatternMapping[group] = append(groupToObjectPatternMapping[group], objectPatterns...)
			}
		}
	}
	return userToObjectPatternMapping, groupToObjectPatternMapping
}
