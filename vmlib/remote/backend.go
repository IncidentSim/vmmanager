package remote

import (
	"context"
	"crypto/tls"

	"google.golang.org/grpc"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	pb "github.com/petruekhin/vibe-firecracker/vmlib/gen/vmlib/backend/v1"
)

// ---- server -----------------------------------------------------------------

type backendServer struct {
	pb.UnimplementedHostBackendServer
	b vmlib.Backend
}

// RegisterBackend exposes a host backend. It is the more privileged listener:
// serve it only on its own address/credentials (default off in vmd).
// Every identifier is validated here before the backend sees it.
func RegisterBackend(s grpc.ServiceRegistrar, b vmlib.Backend) {
	pb.RegisterHostBackendServer(s, &backendServer{b: b})
}

func vmID(id string) error { return vmlib.ValidateID("vm", id) }

func (s *backendServer) Capabilities(ctx context.Context, _ *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	c, err := s.b.Capabilities(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.CapabilitiesResponse{MaxVms: uint32(c.MaxVMs), SshUser: c.SSHUser, SshAuthMode: c.SSHAuthMode,
		MaxTotalMemMib: c.MaxTotalMemMiB, MaxTotalVcpu: c.MaxTotalVCPU}
	for _, p := range c.Profiles {
		out.Profiles = append(out.Profiles, &pb.Profile{Name: p.Name, Vcpu: p.VCPU, MemMib: p.MemMiB, DiskMib: p.DiskMiB})
	}
	if c.Bastion != nil {
		out.Bastion = &pb.Bastion{Host: c.Bastion.Host, Port: uint32(c.Bastion.Port), User: c.Bastion.User}
	}
	return out, nil
}

func imageToPB(i vmlib.Image) *pb.Image {
	return &pb.Image{Id: i.ID, SizeBytes: i.SizeBytes, Sha256: i.SHA256, Base: i.Base}
}

func imageFromPB(i *pb.Image) vmlib.Image {
	return vmlib.Image{ID: i.GetId(), SizeBytes: i.GetSizeBytes(), SHA256: i.GetSha256(), Base: i.GetBase()}
}

func guestToPB(g *vmlib.GuestNetwork) *pb.GuestNetwork {
	if g == nil {
		return nil
	}
	return &pb.GuestNetwork{Address: g.Address, Gateway: g.Gateway, Dns: g.DNS, Configuration: g.Configuration}
}

func guestFromPB(g *pb.GuestNetwork) *vmlib.GuestNetwork {
	if g == nil {
		return nil
	}
	return &vmlib.GuestNetwork{Address: g.GetAddress(), Gateway: g.GetGateway(), DNS: g.GetDns(), Configuration: g.GetConfiguration()}
}

func hostToPB(h vmlib.HostVM) *pb.HostVM {
	return &pb.HostVM{Guest: guestToPB(h.Guest), VmId: h.ID, ImageId: h.ImageID, Profile: h.Profile, State: h.State, GuestIp: h.GuestIP,
		Retained: h.Retained, RetainedReason: h.RetainedReason, SshHostKeyFingerprint: h.HostKeyFingerprint,
		PublicKeyProvisioned: h.PublicKeyProvisioned}
}

func hostFromPB(h *pb.HostVM) vmlib.HostVM {
	return vmlib.HostVM{Guest: guestFromPB(h.GetGuest()), ID: h.GetVmId(), ImageID: h.GetImageId(), Profile: h.GetProfile(), State: h.GetState(),
		GuestIP: h.GetGuestIp(), Retained: h.GetRetained(), RetainedReason: h.GetRetainedReason(),
		HostKeyFingerprint: h.GetSshHostKeyFingerprint(), PublicKeyProvisioned: h.GetPublicKeyProvisioned()}
}

func (s *backendServer) ListImages(ctx context.Context, _ *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	imgs, err := s.b.ListImages(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListImagesResponse{}
	for _, i := range imgs {
		out.Images = append(out.Images, imageToPB(i))
	}
	return out, nil
}

func (s *backendServer) GetImage(ctx context.Context, r *pb.GetImageRequest) (*pb.Image, error) {
	if err := vmlib.ValidateID("image", r.GetId()); err != nil {
		return nil, toStatus(err)
	}
	i, err := s.b.GetImage(ctx, r.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return imageToPB(i), nil
}

func (s *backendServer) Prepare(ctx context.Context, r *pb.PrepareRequest) (*pb.HostVM, error) {
	for _, c := range [][2]string{{"vm", r.GetVmId()}, {"image", r.GetImageId()}, {"profile", r.GetProfile()}} {
		if err := vmlib.ValidateID(c[0], c[1]); err != nil {
			return nil, toStatus(err)
		}
	}
	if err := vmlib.ValidatePublicKey(r.GetSshPublicKey()); err != nil {
		return nil, toStatus(err)
	}
	h, err := s.b.Prepare(ctx, vmlib.PrepareRequest{VMID: r.GetVmId(), ImageID: r.GetImageId(), Profile: r.GetProfile(), SSHPublicKey: r.GetSshPublicKey()})
	if err != nil {
		return nil, toStatus(err)
	}
	return hostToPB(h), nil
}

func (s *backendServer) hostOp(ctx context.Context, id string, f func(context.Context, string) (vmlib.HostVM, error)) (*pb.HostVM, error) {
	if err := vmID(id); err != nil {
		return nil, toStatus(err)
	}
	h, err := f(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	return hostToPB(h), nil
}

func (s *backendServer) Start(ctx context.Context, r *pb.VMRef) (*pb.HostVM, error) {
	return s.hostOp(ctx, r.GetVmId(), s.b.Start)
}
func (s *backendServer) Inspect(ctx context.Context, r *pb.VMRef) (*pb.HostVM, error) {
	return s.hostOp(ctx, r.GetVmId(), s.b.Inspect)
}
func (s *backendServer) RequestShutdown(ctx context.Context, r *pb.VMRef) (*pb.HostVM, error) {
	return s.hostOp(ctx, r.GetVmId(), s.b.RequestShutdown)
}

func (s *backendServer) List(ctx context.Context, _ *pb.ListRequest) (*pb.ListResponse, error) {
	vms, err := s.b.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListResponse{}
	for _, v := range vms {
		out.Vms = append(out.Vms, hostToPB(v))
	}
	return out, nil
}

func (s *backendServer) WaitExit(ctx context.Context, r *pb.VMRef) (*pb.ExitReceipt, error) {
	if err := vmID(r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	rc, err := s.b.WaitExit(ctx, r.GetVmId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ExitReceipt{VmId: rc.VMID, Clean: rc.Clean, Detail: rc.Detail}, nil
}

func (s *backendServer) Publish(ctx context.Context, r *pb.PublishRequest) (*pb.Image, error) {
	if err := vmID(r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	if err := vmlib.ValidateID("image", r.GetNewImageId()); err != nil {
		return nil, toStatus(err)
	}
	i, err := s.b.Publish(ctx, r.GetVmId(), r.GetNewImageId())
	if err != nil {
		return nil, toStatus(err)
	}
	return imageToPB(i), nil
}

func (s *backendServer) Release(ctx context.Context, r *pb.VMRef) (*pb.ReleaseResponse, error) {
	if err := vmID(r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	res, err := s.b.Release(ctx, r.GetVmId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ReleaseResponse{Released: res.Released, Detail: res.Detail}, nil
}

// ---- client -----------------------------------------------------------------

// BackendClient implements vmlib.Backend over gRPC+mTLS.
type BackendClient struct {
	conn *grpc.ClientConn
	c    pb.HostBackendClient
}

var _ vmlib.Backend = (*BackendClient)(nil)

// DialBackend connects lazily; verification happens during the TLS handshake.
func DialBackend(addr string, cfg *tls.Config) (*BackendClient, error) {
	opts, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.Dial(addr, opts...)
	if err != nil {
		return nil, err
	}
	return &BackendClient{conn: conn, c: pb.NewHostBackendClient(conn)}, nil
}

func (c *BackendClient) Close() error { return c.conn.Close() }

func (c *BackendClient) Capabilities(ctx context.Context) (vmlib.Capabilities, error) {
	r, err := c.c.Capabilities(ctx, &pb.CapabilitiesRequest{})
	if err != nil {
		return vmlib.Capabilities{}, fromStatus(err)
	}
	out := vmlib.Capabilities{MaxVMs: int(r.GetMaxVms()), SSHUser: r.GetSshUser(), SSHAuthMode: r.GetSshAuthMode(),
		MaxTotalMemMiB: r.GetMaxTotalMemMib(), MaxTotalVCPU: r.GetMaxTotalVcpu()}
	for _, p := range r.GetProfiles() {
		out.Profiles = append(out.Profiles, vmlib.Profile{Name: p.GetName(), VCPU: p.GetVcpu(), MemMiB: p.GetMemMib(), DiskMiB: p.GetDiskMib()})
	}
	if b := r.GetBastion(); b != nil {
		out.Bastion = &vmlib.Bastion{Host: b.GetHost(), Port: int(b.GetPort()), User: b.GetUser()}
	}
	return out, nil
}

func (c *BackendClient) ListImages(ctx context.Context) ([]vmlib.Image, error) {
	r, err := c.c.ListImages(ctx, &pb.ListImagesRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.Image
	for _, i := range r.GetImages() {
		out = append(out, imageFromPB(i))
	}
	return out, nil
}

func (c *BackendClient) GetImage(ctx context.Context, id string) (vmlib.Image, error) {
	r, err := c.c.GetImage(ctx, &pb.GetImageRequest{Id: id})
	if err != nil {
		return vmlib.Image{}, fromStatus(err)
	}
	return imageFromPB(r), nil
}

func (c *BackendClient) Prepare(ctx context.Context, req vmlib.PrepareRequest) (vmlib.HostVM, error) {
	r, err := c.c.Prepare(ctx, &pb.PrepareRequest{VmId: req.VMID, ImageId: req.ImageID, Profile: req.Profile, SshPublicKey: req.SSHPublicKey})
	if err != nil {
		return vmlib.HostVM{}, fromStatus(err)
	}
	return hostFromPB(r), nil
}

func (c *BackendClient) host(r *pb.HostVM, err error) (vmlib.HostVM, error) {
	if err != nil {
		return vmlib.HostVM{}, fromStatus(err)
	}
	return hostFromPB(r), nil
}

func (c *BackendClient) Start(ctx context.Context, id string) (vmlib.HostVM, error) {
	return c.host(c.c.Start(ctx, &pb.VMRef{VmId: id}))
}
func (c *BackendClient) Inspect(ctx context.Context, id string) (vmlib.HostVM, error) {
	return c.host(c.c.Inspect(ctx, &pb.VMRef{VmId: id}))
}
func (c *BackendClient) RequestShutdown(ctx context.Context, id string) (vmlib.HostVM, error) {
	return c.host(c.c.RequestShutdown(ctx, &pb.VMRef{VmId: id}))
}

func (c *BackendClient) List(ctx context.Context) ([]vmlib.HostVM, error) {
	r, err := c.c.List(ctx, &pb.ListRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.HostVM
	for _, v := range r.GetVms() {
		out = append(out, hostFromPB(v))
	}
	return out, nil
}

func (c *BackendClient) WaitExit(ctx context.Context, id string) (vmlib.ExitReceipt, error) {
	r, err := c.c.WaitExit(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		return vmlib.ExitReceipt{}, fromStatus(err)
	}
	return vmlib.ExitReceipt{VMID: r.GetVmId(), Clean: r.GetClean(), Detail: r.GetDetail()}, nil
}

func (c *BackendClient) Publish(ctx context.Context, id, newID string) (vmlib.Image, error) {
	r, err := c.c.Publish(ctx, &pb.PublishRequest{VmId: id, NewImageId: newID})
	if err != nil {
		return vmlib.Image{}, fromStatus(err)
	}
	return imageFromPB(r), nil
}

func (c *BackendClient) Release(ctx context.Context, id string) (vmlib.ReleaseResult, error) {
	r, err := c.c.Release(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		return vmlib.ReleaseResult{}, fromStatus(err)
	}
	return vmlib.ReleaseResult{Released: r.GetReleased(), Detail: r.GetDetail()}, nil
}
