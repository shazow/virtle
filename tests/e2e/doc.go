// Package e2e boots the fast fixture on real VMMs through the public Go
// API: backend/qemu, backend/firecracker, and backend/cloudhypervisor driven
// by vm.Spec rather than by manifests, so the scenarios exercise what a
// library consumer writes. The tests carry the integration build tag and
// skip without the VIRTLE_E2E_* environment; the e2e-api flake check runs
// them under KVM.
package e2e
