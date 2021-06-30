# CSR approval mechanism for BYOH instances in WMCO

The following criteria should be satisfied for CSR's of BYOH instances to be approved by the WMCO CSR approver:
### Node name validation:
   * Node name present in the CSR subject name should be of the format system:nodes:_\<node_name\>_
   * Node name retrieved from the CSR is future node name of the instance set by the kubelet based on cloud provider spec.
     Therefore node name should match with either one of the two addresses:
       * actual host name of the instance
       * DNS address of the instance

### CSR content validation:        
Kubelet needs two certificates for its normal operation:

   * Client certificate - for securely communicating with the Kubernetes API server
      Node client bootstrapper CSR is the one received before the instance becomes a node and is 
      signed by kube-apiserver-client-kubelet signer.
      
      It is a node client bootstrapper CSR if it matches below criteria:
       *  CSR subject organizations only contain: "system:nodes"
       *  CSR contents do not contain any fields for DNS names, IP address or email addresses.
       *  CSR spec usages contain: "key encipherment", "digital signature", "client auth"
       *  Node object should not already be present for the node name in the cluster.
       
   * Server certificate - for use in its own local https endpoint, used by the API server to talk back to kubelet.
      Node serving CSR is the one received after the instance becomes a node and signed by the kubelet-serving signer.
      
      The node serving CSR is validated based on below criteria:
       * CSR spec groups must contain: "system:nodes", "system:authenticated"
       * CSR spec usages must contain: "key encipherment", "digital signature", "server auth"
       * CSR subject organizations must contain:  "system:nodes"
       