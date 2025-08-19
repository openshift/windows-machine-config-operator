package webhook

import (
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Server manages the WICD admission webhook server
type Server struct {
	mgr            ctrl.Manager
	nodeAdmission  *WICDNodeAdmission
	webhookEnabled bool
}

// NewServer creates a new webhook server
func NewServer(mgr ctrl.Manager, certDir string) *Server {
	return &Server{
		mgr:            mgr,
		nodeAdmission:  NewWICDNodeAdmissionWebhook(),
		webhookEnabled: true,
	}
}

// SetupWithManager sets up the webhook with the manager
func (s *Server) SetupWithManager(mgr ctrl.Manager) error {
	if !s.webhookEnabled {
		ctrl.Log.Info("WICD admission webhook is disabled")
		return nil
	}

	ctrl.Log.Info("Setting up WICD admission webhook")

	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Node{}).
		WithValidator(s.nodeAdmission).
		Complete()
}

// Disable disables the webhook server (useful for testing)
func (s *Server) Disable() {
	s.webhookEnabled = false
}

// IsEnabled returns whether the webhook server is enabled
func (s *Server) IsEnabled() bool {
	return s.webhookEnabled
}

// GetNodeAdmissionWebhook returns the node admission webhook for testing
func (s *Server) GetNodeAdmissionWebhook() *WICDNodeAdmission {
	return s.nodeAdmission
}
