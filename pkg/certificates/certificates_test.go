package certificates

import (
	"reflect"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMergeCAsConfigMaps(t *testing.T) {
	type args struct {
		initialCAConfigMap *core.ConfigMap
		currentCAConfigMap *core.ConfigMap
		subject            string
	}
	tests := []struct {
		name    string
		args    args
		want    []byte
		wantErr bool
	}{
		{
			name: "empty subject",
			args: args{
				nil,
				nil,
				"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid subject and no CA bundles",
			args: args{
				nil,
				nil,
				"kube-apiserver-to-kubelet-signer",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid subject and no current CA bundle",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				nil,
				"kube-apiserver-to-kubelet-signer",
			},
			want:    []byte(testInitialCABundle),
			wantErr: false,
		},
		{
			name: "valid subject and invalid key in current CA bundle",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				createFakeConfigMapFromCABundle("ca-bundle-invalid.crt",
					testRotatedKubeletCACertificate),
				"kube-apiserver-to-kubelet-signer",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid subject and current CA bundle with empty data",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				createFakeConfigMapFromCABundle("ca-bundle.crt", ""),
				"kube-apiserver-to-kubelet-signer",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid subject and valid CA bundles",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				createFakeConfigMapFromCABundle("ca-bundle.crt",
					testRotatedKubeletCACertificate),
				"invalid-subject",
			},
			want:    []byte(testInitialCABundle),
			wantErr: false,
		},
		{
			name: "valid subject and CA bundle",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				createFakeConfigMapFromCABundle("ca-bundle.crt",
					testRotatedKubeletCACertificate),
				"kube-apiserver-to-kubelet-signer",
			},
			want:    []byte(wantCABundleWithRotatedKubeletCertificate),
			wantErr: false,
		},
		{
			name: "valid subject and two certificates in CA bundle",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialCABundle),
				createFakeConfigMapFromCABundle("ca-bundle.crt",
					testCABundleWithInitialAndRotatedCerts),
				"kube-apiserver-to-kubelet-signer",
			},
			want:    []byte(wantCABundleWithInitialAndRotatedKubeletCertificate),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergeCAsConfigMaps(tt.args.initialCAConfigMap, tt.args.currentCAConfigMap, tt.args.subject)
			if (err != nil) != tt.wantErr {
				t.Errorf("MergeCAsConfigMaps() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeCAsConfigMaps() got = %s,\nwant = %s", got, tt.want)
			}
		})
	}
}

func TestGetCAsFromConfigMap(t *testing.T) {
	type args struct {
		configMap *core.ConfigMap
		key       string
	}
	tests := []struct {
		name    string
		args    args
		want    []byte
		wantErr bool
	}{
		{
			name: "no configMap",
			args: args{
				nil,
				"ca-bundle.crt",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "empty key",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialKubeletCACertificate),
				"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid key",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialKubeletCACertificate),
				"invalid-key.crt",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid configMap and key",
			args: args{
				createFakeConfigMapFromCABundle("ca-bundle.crt", testInitialKubeletCACertificate),
				"ca-bundle.crt",
			},
			want:    []byte(testInitialKubeletCACertificate),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetCAsFromConfigMap(tt.args.configMap, tt.args.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetCAsFromConfigMap() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetCAsFromConfigMap() got = %v, want %v", got, tt.want)
			}
		})
	}
}

// createFakeConfigMapFromCABundle returns a fake CA Bundle ConfigMap using the given key and data
func createFakeConfigMapFromCABundle(key, data string) *core.ConfigMap {
	return &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      "fake",
			Namespace: "test",
		},
		Data: map[string]string{key: data},
	}
}

// testInitialCABundle is a fake initial CA bundle consisting of 5 certificates
// Issuer: OU=openshift, CN=admin-kubeconfig-signer                 Not After : Mar 25 14:35:40 2032 GMT	Subject: OU=openshift, CN=admin-kubeconfig-signer
// Issuer: OU=openshift, CN=kubelet-signer                          Not After : Mar 29 14:35:44 2022 GMT	Subject: OU=openshift, CN=kubelet-signer
// Issuer: OU=openshift, CN=kube-control-plane-signer               Not After : Mar 28 14:35:44 2023 GMT	Subject: OU=openshift, CN=kube-control-plane-signer
// Issuer: OU=openshift, CN=kube-apiserver-to-kubelet-signer        Not After : Mar 28 14:35:44 2023 GMT	Subject: OU=openshift, CN=kube-apiserver-to-kubelet-signer
// Issuer: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer     Not After : Mar 25 14:35:41 2032 GMT	Subject: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer
const testInitialCABundle = `-----BEGIN CERTIFICATE-----
MIIDMDCCAhigAwIBAgIIRW3i6I31pXQwDQYJKoZIhvcNAQELBQAwNjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSAwHgYDVQQDExdhZG1pbi1rdWJlY29uZmlnLXNpZ25lcjAe
Fw0yMjAzMjgxNDM1NDBaFw0zMjAzMjUxNDM1NDBaMDYxEjAQBgNVBAsTCW9wZW5z
aGlmdDEgMB4GA1UEAxMXYWRtaW4ta3ViZWNvbmZpZy1zaWduZXIwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDNuhLezsaHl/v8WqAb2PT0jsjJ8SnTKnN8
D2QaLYx2Y95vc+Vf7tq2waEiMZLpCdB4qYYwSVjasTjNu+MitPVXj5XQKDPpE9mM
xTIE2kI2S1fcXerhU45LGWdflF9LSHp9gTOJa+12gEhuIoJLk3ZwQGk0CfKO0q96
RpmiDFoROQCfDYcMHBU8jV3BHAVK+djtywImioggErD1qc+NFRZmk8dyrgxJlARW
C8FUeHnhZ4O3MbzAS9oZ4nNMr74bMZa29j8rkCr3yyg78KVk1piKDnBaUCNBe2eE
iSZW/4SmWi/Cplabp3w6w1XdgNiVV2x+EFsjVnv2q7YAF6rmMn5xAgMBAAGjQjBA
MA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRZGO4q
hl0bzOZY8I/k5xvsWqIFbDANBgkqhkiG9w0BAQsFAAOCAQEACsIvkER8rMulNkaq
4IU6uouzN3mFyKU8h3+t8dpZJn6eAh1ZsdPGAmvW56KI5+cqkNNmEoHFT3LwVgZW
J1b0PsXygmrhcNoKnJQPtvBJoLc0cbZIPo6Qojxqx/R2wnvhwu6VpgpGiKJ2x4KH
SvkMOsGP+bmBAIJTEzoI+7fwooPeoLnhIsTZJVEQR5nkRVa/dKOckZcPWP9943sp
RG5PDTNLoPu3gWmlK/4/IPBxDAXrxsnOosDUQU/TxOdokPizxFC6V85kUZ+ahBgW
xuSsUTze98N6AF+u2u5YVBX5dYmMsyyPgF+ZKURp1WVplQ2gsP+b+DsNyFckR228
49ewWQ==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDHjCCAgagAwIBAgIIFCEQzDowXqYwDQYJKoZIhvcNAQELBQAwLTESMBAGA1UE
CxMJb3BlbnNoaWZ0MRcwFQYDVQQDEw5rdWJlbGV0LXNpZ25lcjAeFw0yMjAzMjgx
NDM1NDRaFw0yMjAzMjkxNDM1NDRaMC0xEjAQBgNVBAsTCW9wZW5zaGlmdDEXMBUG
A1UEAxMOa3ViZWxldC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
AoIBAQCujGZ8Dhl4AAUF9gl5RkjNnDNbwBJVYzRl3/7cBFkc2e4uGYyg71tnbP09
fKJ7rYnQWLYKPyncEEcWju7LuJUB9lrotkW3v+Yp9XCKyG9NAJ7kGOWNJz7Sm8de
mF89kZGfz/5rQvH7pBhsf+AvKxNXrvEmJDM+xURS0rKwsUaE8XTwNaiUrWNUW02E
G6jHlwf3N/+h2HNiEJdxVN62RdOili9eLQF3lPCeM4Tutcmhd/FS7WpY8EMX1daq
YpMC81jzACX0FfwQ/KU8PE/YumozrqKdivWJzlAYq/Fu0YxheC6i9RuuNXJnCVNs
CFjltUqf1tGaaF6iSnjP0PUFl71RAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAP
BgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBSbrGq2yx2BhqM5uSkVBOzPFHTr3jAN
BgkqhkiG9w0BAQsFAAOCAQEAWf60cqkIzkuVrgdOilC+Y0LlLU9fBf1qPm8KjzB5
gams374radxoH4z9kAFEtugcg7a9R9yrHyFaq1fkD9sgT+5CcdYtYsEu8iI4oz2x
Swu6xjI/wsHemYYPNCPTlGyyNyPE2cMl4LF4TnsDVBxUmqalZW5xruYjxf/9/aB0
lfMyS+UkIsC6PxKzD+Wp8rqyv0kZMTJB4JSp+O0aHtod1wV/I7dmAPTXy2j/EsrK
ZYPU4DfynU4AaMtrHOojxaUiZRV1cpkyUb7ZU7MSGzkiDD44pQVcGFDw+kpP8yIi
k198rTVGayy5wSJI33yguVOCMIsy+QY31frBUT1ZEVg47Q==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDNDCCAhygAwIBAgIIT1CgQX1CkvgwDQYJKoZIhvcNAQELBQAwODESMBAGA1UE
CxMJb3BlbnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVy
MB4XDTIyMDMyODE0MzU0NFoXDTIzMDMyODE0MzU0NFowODESMBAGA1UECxMJb3Bl
bnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVyMIIBIjAN
BgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvhee/O/dokO5ceQqiAZRfbZBZZSf
4hxjxktksubr2FZS74JKM+L1P2ezsKW2p/VhIoNJ8i0xgbaRTCr84+nJ0WUJSqrN
QVrbPD0UeX5GxkXTl1nW7oFJtk1ix1SH1VVk9l6K84LkEygF7F/QESQRYeWM95Fd
gblP1Am1lccQty2TU9I3+XyV/KmV+EsSVTMupWhoj/LXc025fB+tRd2cG2tdr98e
ey3c3Oaspdp9/RQ0lLJ4ZYYsjQNQgv4ewUrNS+Hj2KeLWluOEwoOupMmTOmyO9RJ
n/lp8BMEhgNUEgqYAxvysfMfJqfS3OK68yeTiqh8IeeQ698bdjUE1tzQEQIDAQAB
o0IwQDAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQU
9N2RM9m7N/AA6wczEk3haMffSEwwDQYJKoZIhvcNAQELBQADggEBAAEg7mgdAyDj
RXBvy2CB0Nh3aygUpty2XhU6cSt+Lsm1DIzTZTYCnw+9YcbJ16xIq0JrYKDHilT/
pce+jdueCD5/OE6adD7/W4qC9y9JBcGUJDkM23p7g3uep4hMmx0hEn9ulL8bLnf/
phRjfDUW8N3i/R7rX2q2nqgypvDbNPJ1cW0P9g5s9cXf2mCvmSx/tH9/FaadR9CO
RfVKUQ4mENq47JADfW+oSUQX5ta9yDWfD1PrXDcOIoZcdDtquRGe+dkjmKtgjbKV
/wtuFvI9bxJmS8SJygdi0XC3YrXsWaEaDm5wTYPUcFrAkIPMa7hrg8UlgZq4VHW4
ZGloLhNEXy4=
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDQjCCAiqgAwIBAgIIc9V7aeUc/1wwDQYJKoZIhvcNAQELBQAwPzESMBAGA1UE
CxMJb3BlbnNoaWZ0MSkwJwYDVQQDEyBrdWJlLWFwaXNlcnZlci10by1rdWJlbGV0
LXNpZ25lcjAeFw0yMjAzMjgxNDM1NDRaFw0yMzAzMjgxNDM1NDRaMD8xEjAQBgNV
BAsTCW9wZW5zaGlmdDEpMCcGA1UEAxMga3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxl
dC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCjkFdKK9Pk
3YScwmxfLrwXE4ZmdFyLYv0vQ7Jv9l3q5ALE8NlTBCYQQW3NUE3qcGAxz4wr/IoA
JmCqkRJOMlwrEt1GpJhV6Aa6n2RZ6k4kUQsR1ERbyiNBoz00vGx2gi+2pnkkd6W2
8zKd5oU+WH2bSDLzpHjMQ6cqBD58x/FmelUjuizmGyGqPWXAPrh7cX3Uw2/1+OqB
d0zfZsLj9DC9Z5UZcn0tyUqgUfc4W0WRddFMtONVJywaDcumDNPVjY5zu6UqWsG2
AxL7uJBEMaZs+Le9HHGDWZlqHe/5YH1e54qgY+9asc6Z3np9KvZr0SfQiviDYx2B
0tASu0CXeWrVAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTAD
AQH/MB0GA1UdDgQWBBRtaEfiHQvZxHJmj19iG/ZyXabZXjANBgkqhkiG9w0BAQsF
AAOCAQEAHlB0zQDEktLBed9w5SU6S6WNi4g8NAgPh/5ir9inwbuQO/e1bgexFFHX
fvADKOHaRCEDHxMWLJ9fIwt+9pvDlUbOS1/OJiOzHY+PeyCWII2GKTsv+i41WDnm
z49+1h8Bgqm+c0nwbJMT+/fyn9wlPMWat16hZEjoIfYMWkwEb/NOE3EE6ogdcZKV
sXmV3TK4PzxnfE1rcyFHk3vO1Cv3xEpukAFf0alK3XfPdKZB4lUQ5w6ymO4GS47N
GoUioYbGdLm6AeyKMGtsDNdgvxc/rSbHTLr6PCJj/mayYupJLhHLQRJ88RIzUM9F
47bTuKiCHI4paFOs5PIh4+Aqm6eCVA==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDSDCCAjCgAwIBAgIIa14U85gSrrgwDQYJKoZIhvcNAQELBQAwQjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSwwKgYDVQQDEyNrdWJlbGV0LWJvb3RzdHJhcC1rdWJlY29u
ZmlnLXNpZ25lcjAeFw0yMjAzMjgxNDM1NDFaFw0zMjAzMjUxNDM1NDFaMEIxEjAQ
BgNVBAsTCW9wZW5zaGlmdDEsMCoGA1UEAxMja3ViZWxldC1ib290c3RyYXAta3Vi
ZWNvbmZpZy1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQD7
aMQ6ro0PrMizoE2iLBfsCHs9XNNkZskluYbfYJrlr3CGrruo8SNWHF+LRsO06Ly/
F+vTCEY1kxQx6xBXLQRQhyLWQh5Gcn/y4MGatGwTVeo/je4/iPwWn1b0X9y2v61K
SKcrSh8RJZFvuZEGjMpwKq4l3H15qlRLsAkKdW14L8dWhkfB7nA9eYe8eo/tgqLp
r3zxkjnQMOBi0gjjBvoMVWHwiC+K3Ll+MxPoNz0NWp0prqPc9KPuT4Xs5vTAbT5y
Hwi3V0NwqvtsK+qavaMepd8Gwwj47+1OixLyly2bx8zKKfyJIuLuGQfgFV5j/lEo
12crZBKun7BA9betnaRDAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMB
Af8EBTADAQH/MB0GA1UdDgQWBBSRJq/DDn8mIkdff9AHhn3XNFSNWjANBgkqhkiG
9w0BAQsFAAOCAQEAAyNRyk4Kquy/2alCllTldC0kr4ZEo+MMtQAXBYy1LESZpdNc
ntYyOf+dd1SGd0MBtUnmpmzCrJj8Pn4t+d+emuFfyqTO5btRCMgwVkcVHYyBzM0X
LR0Ja8sbPQVKcDpMrqh/4/K1Nr1eBqkwfrOvYyuG8roOGq41fVGqwTRADFIAiOPw
YJBI2QyEdsA6jFFyIxHP34sntvzFsUObL5g6SxsWKHA97FMBklIRz5JSviMG4/kH
725e1njz0NpMxT2vZTO15/VyqW8oXVRT/beN/daIcekLb3pLP1rkWuA9GWnhjEVQ
SupfHrSpy6CQOJNNlDTjGdvJtV7j+uFgrSJhIA==
-----END CERTIFICATE-----
`

// testInitialKubeletCACertificate is a fake certificate that contains an initial CA for
// kube-apiserver-to-kubelet-signer
//  Issuer: OU=openshift, CN=kube-apiserver-to-kubelet-signer	Not After : Mar 28 14:35:44 2023 GMT
const testInitialKubeletCACertificate = `-----BEGIN CERTIFICATE-----
MIIDQjCCAiqgAwIBAgIIc9V7aeUc/1wwDQYJKoZIhvcNAQELBQAwPzESMBAGA1UE
CxMJb3BlbnNoaWZ0MSkwJwYDVQQDEyBrdWJlLWFwaXNlcnZlci10by1rdWJlbGV0
LXNpZ25lcjAeFw0yMjAzMjgxNDM1NDRaFw0yMzAzMjgxNDM1NDRaMD8xEjAQBgNV
BAsTCW9wZW5zaGlmdDEpMCcGA1UEAxMga3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxl
dC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCjkFdKK9Pk
3YScwmxfLrwXE4ZmdFyLYv0vQ7Jv9l3q5ALE8NlTBCYQQW3NUE3qcGAxz4wr/IoA
JmCqkRJOMlwrEt1GpJhV6Aa6n2RZ6k4kUQsR1ERbyiNBoz00vGx2gi+2pnkkd6W2
8zKd5oU+WH2bSDLzpHjMQ6cqBD58x/FmelUjuizmGyGqPWXAPrh7cX3Uw2/1+OqB
d0zfZsLj9DC9Z5UZcn0tyUqgUfc4W0WRddFMtONVJywaDcumDNPVjY5zu6UqWsG2
AxL7uJBEMaZs+Le9HHGDWZlqHe/5YH1e54qgY+9asc6Z3np9KvZr0SfQiviDYx2B
0tASu0CXeWrVAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTAD
AQH/MB0GA1UdDgQWBBRtaEfiHQvZxHJmj19iG/ZyXabZXjANBgkqhkiG9w0BAQsF
AAOCAQEAHlB0zQDEktLBed9w5SU6S6WNi4g8NAgPh/5ir9inwbuQO/e1bgexFFHX
fvADKOHaRCEDHxMWLJ9fIwt+9pvDlUbOS1/OJiOzHY+PeyCWII2GKTsv+i41WDnm
z49+1h8Bgqm+c0nwbJMT+/fyn9wlPMWat16hZEjoIfYMWkwEb/NOE3EE6ogdcZKV
sXmV3TK4PzxnfE1rcyFHk3vO1Cv3xEpukAFf0alK3XfPdKZB4lUQ5w6ymO4GS47N
GoUioYbGdLm6AeyKMGtsDNdgvxc/rSbHTLr6PCJj/mayYupJLhHLQRJ88RIzUM9F
47bTuKiCHI4paFOs5PIh4+Aqm6eCVA==
-----END CERTIFICATE-----
`

// testRotatedKubeletCACertificate is a fake certificate that contains a rotated CA for kube-apiserver-to-kubelet-signer
// Issuer: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648497173		Not After : Mar 28 19:52:53 2023 GMT	Subject: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648497173
const testRotatedKubeletCACertificate = `-----BEGIN CERTIFICATE-----
MIIDlTCCAn2gAwIBAgIIUnXNjNr5QGkwDQYJKoZIhvcNAQELBQAwWDFWMFQGA1UE
AwxNb3BlbnNoaWZ0LWt1YmUtYXBpc2VydmVyLW9wZXJhdG9yX2t1YmUtYXBpc2Vy
dmVyLXRvLWt1YmVsZXQtc2lnbmVyQDE2NDg0OTcxNzMwHhcNMjIwMzI4MTk1MjUy
WhcNMjMwMzI4MTk1MjUzWjBYMVYwVAYDVQQDDE1vcGVuc2hpZnQta3ViZS1hcGlz
ZXJ2ZXItb3BlcmF0b3Jfa3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxldC1zaWduZXJA
MTY0ODQ5NzE3MzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAL76ejm9
VWbE8JTv5ho6ocoS6XuuDYRXfJ0CLfJxzjcWwf3gvMEl45CmmTtpKv4jtL1t0Y0S
w3WLDEkt9B7Wcb0mR3sB6XCI8kETy/Qe+PNLFz7z3SRvVzreAPs3YxBecUIPyg7w
G8xW7xm/auhr+yJq2lckhnLY+76kyIGV2Sik3O2fr6llP1V27Fq5+++SUdVN9GY4
wyhne//CCFR16/WCik+vmQ0IUrLXyeK708i3e1Kz26AxT8GPkNnupnMjWqqCWLEd
JRevb6H5UXecYR6a3L4z44d6z0/5J1Jde4ZOenMwhSZyZoVR8QdH3S0/Ju4Z330D
6fbrnq0JmzNRDE0CAwEAAaNjMGEwDgYDVR0PAQH/BAQDAgKkMA8GA1UdEwEB/wQF
MAMBAf8wHQYDVR0OBBYEFEc8O48bLGkoWkv4TtapUT74zHUrMB8GA1UdIwQYMBaA
FEc8O48bLGkoWkv4TtapUT74zHUrMA0GCSqGSIb3DQEBCwUAA4IBAQAz11wzPQjX
6v7P0vG9qf4Q95Pn5E8p/7osn/jUCpJY64xLTPncS9h6lEAQylsgCzn1E2TWo5cs
NqWCX52qHzliKifIE271RrbfZzuy8XN915FRNImfm+Jd5M7rbLh8ALQ9O/PuVzxM
EfSxF3Z+wisZQXSec6gbQkTKLvQh8GOvMYIz3S/GORQyBStHj2olOeR1Vn1lA5uY
B4NdsBqzVyjsdz7kgpTpYXbRplmNyZdT68tpcoMZ4MzGXUFDYbRhIoSB+ZJWRA7s
JAWZkkUw1Ggun7cv2MbVKoQ5/Lkn6dyWyg0qEqLRJAsAqwigmxynqUABTDP1a4Gb
IRon2qPyNbVV
-----END CERTIFICATE-----
`

// testCABundleWithInitialAndRotatedCerts is a fake CA bundle consisting of 2 certificates, the initial
// kube-apiserver-to-kubelet-signer certificate and a valid certificate originated from a rotation.
const testCABundleWithInitialAndRotatedCerts = testInitialKubeletCACertificate + testRotatedKubeletCACertificate

// wantCABundleWithInitialAndRotatedKubeletCertificate is a fake CA bundle consisting of 6 certificates, where there
// are two kubelet CA certificates (kube-apiserver-to-kubelet-signer) corresponding to the initial and rotated
// certificates. This case is a valid configuration for the CA bundle between the first rotation (80% of expiration) and
// the moment the old certificate is removed (1 year, 100% of expiration).
// Issuer: OU=openshift, CN=admin-kubeconfig-signer                                             Not After : Mar 25 14:35:40 2032 GMT	Subject: OU=openshift, CN=admin-kubeconfig-signer
// Issuer: OU=openshift, CN=kubelet-signer                                                      Not After : Mar 29 14:35:44 2022 GMT	Subject: OU=openshift, CN=kubelet-signer
// Issuer: OU=openshift, CN=kube-control-plane-signer                                           Not After : Mar 28 14:35:44 2023 GMT	Subject: OU=openshift, CN=kube-control-plane-signer
// Issuer: OU=openshift, CN=kube-apiserver-to-kubelet-signer                                    Not After : Mar 28 14:35:44 2023 GMT	Subject: OU=openshift, CN=kube-apiserver-to-kubelet-signer
// Issuer: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648499657     Not After : Mar 28 20:34:17 2023 GMT	Subject: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648499657
// Issuer: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer                                 Not After : Mar 25 14:35:41 2032 GMT	Subject: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer
const wantCABundleWithInitialAndRotatedKubeletCertificate = `-----BEGIN CERTIFICATE-----
MIIDMDCCAhigAwIBAgIIRW3i6I31pXQwDQYJKoZIhvcNAQELBQAwNjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSAwHgYDVQQDExdhZG1pbi1rdWJlY29uZmlnLXNpZ25lcjAe
Fw0yMjAzMjgxNDM1NDBaFw0zMjAzMjUxNDM1NDBaMDYxEjAQBgNVBAsTCW9wZW5z
aGlmdDEgMB4GA1UEAxMXYWRtaW4ta3ViZWNvbmZpZy1zaWduZXIwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDNuhLezsaHl/v8WqAb2PT0jsjJ8SnTKnN8
D2QaLYx2Y95vc+Vf7tq2waEiMZLpCdB4qYYwSVjasTjNu+MitPVXj5XQKDPpE9mM
xTIE2kI2S1fcXerhU45LGWdflF9LSHp9gTOJa+12gEhuIoJLk3ZwQGk0CfKO0q96
RpmiDFoROQCfDYcMHBU8jV3BHAVK+djtywImioggErD1qc+NFRZmk8dyrgxJlARW
C8FUeHnhZ4O3MbzAS9oZ4nNMr74bMZa29j8rkCr3yyg78KVk1piKDnBaUCNBe2eE
iSZW/4SmWi/Cplabp3w6w1XdgNiVV2x+EFsjVnv2q7YAF6rmMn5xAgMBAAGjQjBA
MA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRZGO4q
hl0bzOZY8I/k5xvsWqIFbDANBgkqhkiG9w0BAQsFAAOCAQEACsIvkER8rMulNkaq
4IU6uouzN3mFyKU8h3+t8dpZJn6eAh1ZsdPGAmvW56KI5+cqkNNmEoHFT3LwVgZW
J1b0PsXygmrhcNoKnJQPtvBJoLc0cbZIPo6Qojxqx/R2wnvhwu6VpgpGiKJ2x4KH
SvkMOsGP+bmBAIJTEzoI+7fwooPeoLnhIsTZJVEQR5nkRVa/dKOckZcPWP9943sp
RG5PDTNLoPu3gWmlK/4/IPBxDAXrxsnOosDUQU/TxOdokPizxFC6V85kUZ+ahBgW
xuSsUTze98N6AF+u2u5YVBX5dYmMsyyPgF+ZKURp1WVplQ2gsP+b+DsNyFckR228
49ewWQ==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDHjCCAgagAwIBAgIIFCEQzDowXqYwDQYJKoZIhvcNAQELBQAwLTESMBAGA1UE
CxMJb3BlbnNoaWZ0MRcwFQYDVQQDEw5rdWJlbGV0LXNpZ25lcjAeFw0yMjAzMjgx
NDM1NDRaFw0yMjAzMjkxNDM1NDRaMC0xEjAQBgNVBAsTCW9wZW5zaGlmdDEXMBUG
A1UEAxMOa3ViZWxldC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
AoIBAQCujGZ8Dhl4AAUF9gl5RkjNnDNbwBJVYzRl3/7cBFkc2e4uGYyg71tnbP09
fKJ7rYnQWLYKPyncEEcWju7LuJUB9lrotkW3v+Yp9XCKyG9NAJ7kGOWNJz7Sm8de
mF89kZGfz/5rQvH7pBhsf+AvKxNXrvEmJDM+xURS0rKwsUaE8XTwNaiUrWNUW02E
G6jHlwf3N/+h2HNiEJdxVN62RdOili9eLQF3lPCeM4Tutcmhd/FS7WpY8EMX1daq
YpMC81jzACX0FfwQ/KU8PE/YumozrqKdivWJzlAYq/Fu0YxheC6i9RuuNXJnCVNs
CFjltUqf1tGaaF6iSnjP0PUFl71RAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAP
BgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBSbrGq2yx2BhqM5uSkVBOzPFHTr3jAN
BgkqhkiG9w0BAQsFAAOCAQEAWf60cqkIzkuVrgdOilC+Y0LlLU9fBf1qPm8KjzB5
gams374radxoH4z9kAFEtugcg7a9R9yrHyFaq1fkD9sgT+5CcdYtYsEu8iI4oz2x
Swu6xjI/wsHemYYPNCPTlGyyNyPE2cMl4LF4TnsDVBxUmqalZW5xruYjxf/9/aB0
lfMyS+UkIsC6PxKzD+Wp8rqyv0kZMTJB4JSp+O0aHtod1wV/I7dmAPTXy2j/EsrK
ZYPU4DfynU4AaMtrHOojxaUiZRV1cpkyUb7ZU7MSGzkiDD44pQVcGFDw+kpP8yIi
k198rTVGayy5wSJI33yguVOCMIsy+QY31frBUT1ZEVg47Q==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDNDCCAhygAwIBAgIIT1CgQX1CkvgwDQYJKoZIhvcNAQELBQAwODESMBAGA1UE
CxMJb3BlbnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVy
MB4XDTIyMDMyODE0MzU0NFoXDTIzMDMyODE0MzU0NFowODESMBAGA1UECxMJb3Bl
bnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVyMIIBIjAN
BgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvhee/O/dokO5ceQqiAZRfbZBZZSf
4hxjxktksubr2FZS74JKM+L1P2ezsKW2p/VhIoNJ8i0xgbaRTCr84+nJ0WUJSqrN
QVrbPD0UeX5GxkXTl1nW7oFJtk1ix1SH1VVk9l6K84LkEygF7F/QESQRYeWM95Fd
gblP1Am1lccQty2TU9I3+XyV/KmV+EsSVTMupWhoj/LXc025fB+tRd2cG2tdr98e
ey3c3Oaspdp9/RQ0lLJ4ZYYsjQNQgv4ewUrNS+Hj2KeLWluOEwoOupMmTOmyO9RJ
n/lp8BMEhgNUEgqYAxvysfMfJqfS3OK68yeTiqh8IeeQ698bdjUE1tzQEQIDAQAB
o0IwQDAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQU
9N2RM9m7N/AA6wczEk3haMffSEwwDQYJKoZIhvcNAQELBQADggEBAAEg7mgdAyDj
RXBvy2CB0Nh3aygUpty2XhU6cSt+Lsm1DIzTZTYCnw+9YcbJ16xIq0JrYKDHilT/
pce+jdueCD5/OE6adD7/W4qC9y9JBcGUJDkM23p7g3uep4hMmx0hEn9ulL8bLnf/
phRjfDUW8N3i/R7rX2q2nqgypvDbNPJ1cW0P9g5s9cXf2mCvmSx/tH9/FaadR9CO
RfVKUQ4mENq47JADfW+oSUQX5ta9yDWfD1PrXDcOIoZcdDtquRGe+dkjmKtgjbKV
/wtuFvI9bxJmS8SJygdi0XC3YrXsWaEaDm5wTYPUcFrAkIPMa7hrg8UlgZq4VHW4
ZGloLhNEXy4=
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDQjCCAiqgAwIBAgIIc9V7aeUc/1wwDQYJKoZIhvcNAQELBQAwPzESMBAGA1UE
CxMJb3BlbnNoaWZ0MSkwJwYDVQQDEyBrdWJlLWFwaXNlcnZlci10by1rdWJlbGV0
LXNpZ25lcjAeFw0yMjAzMjgxNDM1NDRaFw0yMzAzMjgxNDM1NDRaMD8xEjAQBgNV
BAsTCW9wZW5zaGlmdDEpMCcGA1UEAxMga3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxl
dC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCjkFdKK9Pk
3YScwmxfLrwXE4ZmdFyLYv0vQ7Jv9l3q5ALE8NlTBCYQQW3NUE3qcGAxz4wr/IoA
JmCqkRJOMlwrEt1GpJhV6Aa6n2RZ6k4kUQsR1ERbyiNBoz00vGx2gi+2pnkkd6W2
8zKd5oU+WH2bSDLzpHjMQ6cqBD58x/FmelUjuizmGyGqPWXAPrh7cX3Uw2/1+OqB
d0zfZsLj9DC9Z5UZcn0tyUqgUfc4W0WRddFMtONVJywaDcumDNPVjY5zu6UqWsG2
AxL7uJBEMaZs+Le9HHGDWZlqHe/5YH1e54qgY+9asc6Z3np9KvZr0SfQiviDYx2B
0tASu0CXeWrVAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTAD
AQH/MB0GA1UdDgQWBBRtaEfiHQvZxHJmj19iG/ZyXabZXjANBgkqhkiG9w0BAQsF
AAOCAQEAHlB0zQDEktLBed9w5SU6S6WNi4g8NAgPh/5ir9inwbuQO/e1bgexFFHX
fvADKOHaRCEDHxMWLJ9fIwt+9pvDlUbOS1/OJiOzHY+PeyCWII2GKTsv+i41WDnm
z49+1h8Bgqm+c0nwbJMT+/fyn9wlPMWat16hZEjoIfYMWkwEb/NOE3EE6ogdcZKV
sXmV3TK4PzxnfE1rcyFHk3vO1Cv3xEpukAFf0alK3XfPdKZB4lUQ5w6ymO4GS47N
GoUioYbGdLm6AeyKMGtsDNdgvxc/rSbHTLr6PCJj/mayYupJLhHLQRJ88RIzUM9F
47bTuKiCHI4paFOs5PIh4+Aqm6eCVA==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDlTCCAn2gAwIBAgIIUnXNjNr5QGkwDQYJKoZIhvcNAQELBQAwWDFWMFQGA1UE
AwxNb3BlbnNoaWZ0LWt1YmUtYXBpc2VydmVyLW9wZXJhdG9yX2t1YmUtYXBpc2Vy
dmVyLXRvLWt1YmVsZXQtc2lnbmVyQDE2NDg0OTcxNzMwHhcNMjIwMzI4MTk1MjUy
WhcNMjMwMzI4MTk1MjUzWjBYMVYwVAYDVQQDDE1vcGVuc2hpZnQta3ViZS1hcGlz
ZXJ2ZXItb3BlcmF0b3Jfa3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxldC1zaWduZXJA
MTY0ODQ5NzE3MzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAL76ejm9
VWbE8JTv5ho6ocoS6XuuDYRXfJ0CLfJxzjcWwf3gvMEl45CmmTtpKv4jtL1t0Y0S
w3WLDEkt9B7Wcb0mR3sB6XCI8kETy/Qe+PNLFz7z3SRvVzreAPs3YxBecUIPyg7w
G8xW7xm/auhr+yJq2lckhnLY+76kyIGV2Sik3O2fr6llP1V27Fq5+++SUdVN9GY4
wyhne//CCFR16/WCik+vmQ0IUrLXyeK708i3e1Kz26AxT8GPkNnupnMjWqqCWLEd
JRevb6H5UXecYR6a3L4z44d6z0/5J1Jde4ZOenMwhSZyZoVR8QdH3S0/Ju4Z330D
6fbrnq0JmzNRDE0CAwEAAaNjMGEwDgYDVR0PAQH/BAQDAgKkMA8GA1UdEwEB/wQF
MAMBAf8wHQYDVR0OBBYEFEc8O48bLGkoWkv4TtapUT74zHUrMB8GA1UdIwQYMBaA
FEc8O48bLGkoWkv4TtapUT74zHUrMA0GCSqGSIb3DQEBCwUAA4IBAQAz11wzPQjX
6v7P0vG9qf4Q95Pn5E8p/7osn/jUCpJY64xLTPncS9h6lEAQylsgCzn1E2TWo5cs
NqWCX52qHzliKifIE271RrbfZzuy8XN915FRNImfm+Jd5M7rbLh8ALQ9O/PuVzxM
EfSxF3Z+wisZQXSec6gbQkTKLvQh8GOvMYIz3S/GORQyBStHj2olOeR1Vn1lA5uY
B4NdsBqzVyjsdz7kgpTpYXbRplmNyZdT68tpcoMZ4MzGXUFDYbRhIoSB+ZJWRA7s
JAWZkkUw1Ggun7cv2MbVKoQ5/Lkn6dyWyg0qEqLRJAsAqwigmxynqUABTDP1a4Gb
IRon2qPyNbVV
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDSDCCAjCgAwIBAgIIa14U85gSrrgwDQYJKoZIhvcNAQELBQAwQjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSwwKgYDVQQDEyNrdWJlbGV0LWJvb3RzdHJhcC1rdWJlY29u
ZmlnLXNpZ25lcjAeFw0yMjAzMjgxNDM1NDFaFw0zMjAzMjUxNDM1NDFaMEIxEjAQ
BgNVBAsTCW9wZW5zaGlmdDEsMCoGA1UEAxMja3ViZWxldC1ib290c3RyYXAta3Vi
ZWNvbmZpZy1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQD7
aMQ6ro0PrMizoE2iLBfsCHs9XNNkZskluYbfYJrlr3CGrruo8SNWHF+LRsO06Ly/
F+vTCEY1kxQx6xBXLQRQhyLWQh5Gcn/y4MGatGwTVeo/je4/iPwWn1b0X9y2v61K
SKcrSh8RJZFvuZEGjMpwKq4l3H15qlRLsAkKdW14L8dWhkfB7nA9eYe8eo/tgqLp
r3zxkjnQMOBi0gjjBvoMVWHwiC+K3Ll+MxPoNz0NWp0prqPc9KPuT4Xs5vTAbT5y
Hwi3V0NwqvtsK+qavaMepd8Gwwj47+1OixLyly2bx8zKKfyJIuLuGQfgFV5j/lEo
12crZBKun7BA9betnaRDAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMB
Af8EBTADAQH/MB0GA1UdDgQWBBSRJq/DDn8mIkdff9AHhn3XNFSNWjANBgkqhkiG
9w0BAQsFAAOCAQEAAyNRyk4Kquy/2alCllTldC0kr4ZEo+MMtQAXBYy1LESZpdNc
ntYyOf+dd1SGd0MBtUnmpmzCrJj8Pn4t+d+emuFfyqTO5btRCMgwVkcVHYyBzM0X
LR0Ja8sbPQVKcDpMrqh/4/K1Nr1eBqkwfrOvYyuG8roOGq41fVGqwTRADFIAiOPw
YJBI2QyEdsA6jFFyIxHP34sntvzFsUObL5g6SxsWKHA97FMBklIRz5JSviMG4/kH
725e1njz0NpMxT2vZTO15/VyqW8oXVRT/beN/daIcekLb3pLP1rkWuA9GWnhjEVQ
SupfHrSpy6CQOJNNlDTjGdvJtV7j+uFgrSJhIA==
-----END CERTIFICATE-----
`

// wantCABundleWithRotatedKubeletCertificate is a fake CA bundle consisting of 5 certificates, where the kubelet CA
// certificate (kube-apiserver-to-kubelet-signer) corresponds to a rotated certificate.
// Issuer: OU=openshift, CN=admin-kubeconfig-signer                                             Not After : Mar 25 14:35:40 2032 GMT	Subject: OU=openshift, CN=admin-kubeconfig-signer
// Issuer: OU=openshift, CN=kubelet-signer                                                      Not After : Mar 29 14:35:44 2022 GMT	Subject: OU=openshift, CN=kubelet-signer
// Issuer: OU=openshift, CN=kube-control-plane-signer                                           Not After : Mar 28 14:35:44 2023 GMT	Subject: OU=openshift, CN=kube-control-plane-signer
// Issuer: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648497173     Not After : Mar 28 20:34:17 2023 GMT	Subject: CN=openshift-kube-apiserver-operator_kube-apiserver-to-kubelet-signer@1648499657
// Issuer: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer                                 Not After : Mar 25 14:35:41 2032 GMT	Subject: OU=openshift, CN=kubelet-bootstrap-kubeconfig-signer
const wantCABundleWithRotatedKubeletCertificate = `-----BEGIN CERTIFICATE-----
MIIDMDCCAhigAwIBAgIIRW3i6I31pXQwDQYJKoZIhvcNAQELBQAwNjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSAwHgYDVQQDExdhZG1pbi1rdWJlY29uZmlnLXNpZ25lcjAe
Fw0yMjAzMjgxNDM1NDBaFw0zMjAzMjUxNDM1NDBaMDYxEjAQBgNVBAsTCW9wZW5z
aGlmdDEgMB4GA1UEAxMXYWRtaW4ta3ViZWNvbmZpZy1zaWduZXIwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDNuhLezsaHl/v8WqAb2PT0jsjJ8SnTKnN8
D2QaLYx2Y95vc+Vf7tq2waEiMZLpCdB4qYYwSVjasTjNu+MitPVXj5XQKDPpE9mM
xTIE2kI2S1fcXerhU45LGWdflF9LSHp9gTOJa+12gEhuIoJLk3ZwQGk0CfKO0q96
RpmiDFoROQCfDYcMHBU8jV3BHAVK+djtywImioggErD1qc+NFRZmk8dyrgxJlARW
C8FUeHnhZ4O3MbzAS9oZ4nNMr74bMZa29j8rkCr3yyg78KVk1piKDnBaUCNBe2eE
iSZW/4SmWi/Cplabp3w6w1XdgNiVV2x+EFsjVnv2q7YAF6rmMn5xAgMBAAGjQjBA
MA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRZGO4q
hl0bzOZY8I/k5xvsWqIFbDANBgkqhkiG9w0BAQsFAAOCAQEACsIvkER8rMulNkaq
4IU6uouzN3mFyKU8h3+t8dpZJn6eAh1ZsdPGAmvW56KI5+cqkNNmEoHFT3LwVgZW
J1b0PsXygmrhcNoKnJQPtvBJoLc0cbZIPo6Qojxqx/R2wnvhwu6VpgpGiKJ2x4KH
SvkMOsGP+bmBAIJTEzoI+7fwooPeoLnhIsTZJVEQR5nkRVa/dKOckZcPWP9943sp
RG5PDTNLoPu3gWmlK/4/IPBxDAXrxsnOosDUQU/TxOdokPizxFC6V85kUZ+ahBgW
xuSsUTze98N6AF+u2u5YVBX5dYmMsyyPgF+ZKURp1WVplQ2gsP+b+DsNyFckR228
49ewWQ==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDHjCCAgagAwIBAgIIFCEQzDowXqYwDQYJKoZIhvcNAQELBQAwLTESMBAGA1UE
CxMJb3BlbnNoaWZ0MRcwFQYDVQQDEw5rdWJlbGV0LXNpZ25lcjAeFw0yMjAzMjgx
NDM1NDRaFw0yMjAzMjkxNDM1NDRaMC0xEjAQBgNVBAsTCW9wZW5zaGlmdDEXMBUG
A1UEAxMOa3ViZWxldC1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
AoIBAQCujGZ8Dhl4AAUF9gl5RkjNnDNbwBJVYzRl3/7cBFkc2e4uGYyg71tnbP09
fKJ7rYnQWLYKPyncEEcWju7LuJUB9lrotkW3v+Yp9XCKyG9NAJ7kGOWNJz7Sm8de
mF89kZGfz/5rQvH7pBhsf+AvKxNXrvEmJDM+xURS0rKwsUaE8XTwNaiUrWNUW02E
G6jHlwf3N/+h2HNiEJdxVN62RdOili9eLQF3lPCeM4Tutcmhd/FS7WpY8EMX1daq
YpMC81jzACX0FfwQ/KU8PE/YumozrqKdivWJzlAYq/Fu0YxheC6i9RuuNXJnCVNs
CFjltUqf1tGaaF6iSnjP0PUFl71RAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAP
BgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBSbrGq2yx2BhqM5uSkVBOzPFHTr3jAN
BgkqhkiG9w0BAQsFAAOCAQEAWf60cqkIzkuVrgdOilC+Y0LlLU9fBf1qPm8KjzB5
gams374radxoH4z9kAFEtugcg7a9R9yrHyFaq1fkD9sgT+5CcdYtYsEu8iI4oz2x
Swu6xjI/wsHemYYPNCPTlGyyNyPE2cMl4LF4TnsDVBxUmqalZW5xruYjxf/9/aB0
lfMyS+UkIsC6PxKzD+Wp8rqyv0kZMTJB4JSp+O0aHtod1wV/I7dmAPTXy2j/EsrK
ZYPU4DfynU4AaMtrHOojxaUiZRV1cpkyUb7ZU7MSGzkiDD44pQVcGFDw+kpP8yIi
k198rTVGayy5wSJI33yguVOCMIsy+QY31frBUT1ZEVg47Q==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDNDCCAhygAwIBAgIIT1CgQX1CkvgwDQYJKoZIhvcNAQELBQAwODESMBAGA1UE
CxMJb3BlbnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVy
MB4XDTIyMDMyODE0MzU0NFoXDTIzMDMyODE0MzU0NFowODESMBAGA1UECxMJb3Bl
bnNoaWZ0MSIwIAYDVQQDExlrdWJlLWNvbnRyb2wtcGxhbmUtc2lnbmVyMIIBIjAN
BgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvhee/O/dokO5ceQqiAZRfbZBZZSf
4hxjxktksubr2FZS74JKM+L1P2ezsKW2p/VhIoNJ8i0xgbaRTCr84+nJ0WUJSqrN
QVrbPD0UeX5GxkXTl1nW7oFJtk1ix1SH1VVk9l6K84LkEygF7F/QESQRYeWM95Fd
gblP1Am1lccQty2TU9I3+XyV/KmV+EsSVTMupWhoj/LXc025fB+tRd2cG2tdr98e
ey3c3Oaspdp9/RQ0lLJ4ZYYsjQNQgv4ewUrNS+Hj2KeLWluOEwoOupMmTOmyO9RJ
n/lp8BMEhgNUEgqYAxvysfMfJqfS3OK68yeTiqh8IeeQ698bdjUE1tzQEQIDAQAB
o0IwQDAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQU
9N2RM9m7N/AA6wczEk3haMffSEwwDQYJKoZIhvcNAQELBQADggEBAAEg7mgdAyDj
RXBvy2CB0Nh3aygUpty2XhU6cSt+Lsm1DIzTZTYCnw+9YcbJ16xIq0JrYKDHilT/
pce+jdueCD5/OE6adD7/W4qC9y9JBcGUJDkM23p7g3uep4hMmx0hEn9ulL8bLnf/
phRjfDUW8N3i/R7rX2q2nqgypvDbNPJ1cW0P9g5s9cXf2mCvmSx/tH9/FaadR9CO
RfVKUQ4mENq47JADfW+oSUQX5ta9yDWfD1PrXDcOIoZcdDtquRGe+dkjmKtgjbKV
/wtuFvI9bxJmS8SJygdi0XC3YrXsWaEaDm5wTYPUcFrAkIPMa7hrg8UlgZq4VHW4
ZGloLhNEXy4=
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDlTCCAn2gAwIBAgIIUnXNjNr5QGkwDQYJKoZIhvcNAQELBQAwWDFWMFQGA1UE
AwxNb3BlbnNoaWZ0LWt1YmUtYXBpc2VydmVyLW9wZXJhdG9yX2t1YmUtYXBpc2Vy
dmVyLXRvLWt1YmVsZXQtc2lnbmVyQDE2NDg0OTcxNzMwHhcNMjIwMzI4MTk1MjUy
WhcNMjMwMzI4MTk1MjUzWjBYMVYwVAYDVQQDDE1vcGVuc2hpZnQta3ViZS1hcGlz
ZXJ2ZXItb3BlcmF0b3Jfa3ViZS1hcGlzZXJ2ZXItdG8ta3ViZWxldC1zaWduZXJA
MTY0ODQ5NzE3MzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAL76ejm9
VWbE8JTv5ho6ocoS6XuuDYRXfJ0CLfJxzjcWwf3gvMEl45CmmTtpKv4jtL1t0Y0S
w3WLDEkt9B7Wcb0mR3sB6XCI8kETy/Qe+PNLFz7z3SRvVzreAPs3YxBecUIPyg7w
G8xW7xm/auhr+yJq2lckhnLY+76kyIGV2Sik3O2fr6llP1V27Fq5+++SUdVN9GY4
wyhne//CCFR16/WCik+vmQ0IUrLXyeK708i3e1Kz26AxT8GPkNnupnMjWqqCWLEd
JRevb6H5UXecYR6a3L4z44d6z0/5J1Jde4ZOenMwhSZyZoVR8QdH3S0/Ju4Z330D
6fbrnq0JmzNRDE0CAwEAAaNjMGEwDgYDVR0PAQH/BAQDAgKkMA8GA1UdEwEB/wQF
MAMBAf8wHQYDVR0OBBYEFEc8O48bLGkoWkv4TtapUT74zHUrMB8GA1UdIwQYMBaA
FEc8O48bLGkoWkv4TtapUT74zHUrMA0GCSqGSIb3DQEBCwUAA4IBAQAz11wzPQjX
6v7P0vG9qf4Q95Pn5E8p/7osn/jUCpJY64xLTPncS9h6lEAQylsgCzn1E2TWo5cs
NqWCX52qHzliKifIE271RrbfZzuy8XN915FRNImfm+Jd5M7rbLh8ALQ9O/PuVzxM
EfSxF3Z+wisZQXSec6gbQkTKLvQh8GOvMYIz3S/GORQyBStHj2olOeR1Vn1lA5uY
B4NdsBqzVyjsdz7kgpTpYXbRplmNyZdT68tpcoMZ4MzGXUFDYbRhIoSB+ZJWRA7s
JAWZkkUw1Ggun7cv2MbVKoQ5/Lkn6dyWyg0qEqLRJAsAqwigmxynqUABTDP1a4Gb
IRon2qPyNbVV
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDSDCCAjCgAwIBAgIIa14U85gSrrgwDQYJKoZIhvcNAQELBQAwQjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSwwKgYDVQQDEyNrdWJlbGV0LWJvb3RzdHJhcC1rdWJlY29u
ZmlnLXNpZ25lcjAeFw0yMjAzMjgxNDM1NDFaFw0zMjAzMjUxNDM1NDFaMEIxEjAQ
BgNVBAsTCW9wZW5zaGlmdDEsMCoGA1UEAxMja3ViZWxldC1ib290c3RyYXAta3Vi
ZWNvbmZpZy1zaWduZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQD7
aMQ6ro0PrMizoE2iLBfsCHs9XNNkZskluYbfYJrlr3CGrruo8SNWHF+LRsO06Ly/
F+vTCEY1kxQx6xBXLQRQhyLWQh5Gcn/y4MGatGwTVeo/je4/iPwWn1b0X9y2v61K
SKcrSh8RJZFvuZEGjMpwKq4l3H15qlRLsAkKdW14L8dWhkfB7nA9eYe8eo/tgqLp
r3zxkjnQMOBi0gjjBvoMVWHwiC+K3Ll+MxPoNz0NWp0prqPc9KPuT4Xs5vTAbT5y
Hwi3V0NwqvtsK+qavaMepd8Gwwj47+1OixLyly2bx8zKKfyJIuLuGQfgFV5j/lEo
12crZBKun7BA9betnaRDAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMB
Af8EBTADAQH/MB0GA1UdDgQWBBSRJq/DDn8mIkdff9AHhn3XNFSNWjANBgkqhkiG
9w0BAQsFAAOCAQEAAyNRyk4Kquy/2alCllTldC0kr4ZEo+MMtQAXBYy1LESZpdNc
ntYyOf+dd1SGd0MBtUnmpmzCrJj8Pn4t+d+emuFfyqTO5btRCMgwVkcVHYyBzM0X
LR0Ja8sbPQVKcDpMrqh/4/K1Nr1eBqkwfrOvYyuG8roOGq41fVGqwTRADFIAiOPw
YJBI2QyEdsA6jFFyIxHP34sntvzFsUObL5g6SxsWKHA97FMBklIRz5JSviMG4/kH
725e1njz0NpMxT2vZTO15/VyqW8oXVRT/beN/daIcekLb3pLP1rkWuA9GWnhjEVQ
SupfHrSpy6CQOJNNlDTjGdvJtV7j+uFgrSJhIA==
-----END CERTIFICATE-----
`
