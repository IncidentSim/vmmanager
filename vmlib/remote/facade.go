package remote

import (
	"context"
	"crypto/tls"
	"sync"

	"google.golang.org/grpc"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	pb "github.com/petruekhin/vibe-firecracker/vmlib/gen/vmlib/facade/v1"
)

// ---- conversions -------------------------------------------------------------

func vmToPB(v vmlib.VM) *pb.VM {
	return &pb.VM{VmId: v.ID, ImageId: v.ImageID, Profile: v.Profile, State: string(v.State), Labels: v.Labels,
		Retained: v.Retained, RetainedReason: v.RetainedReason}
}

func vmFromPB(v *pb.VM) vmlib.VM {
	return vmlib.VM{ID: v.GetVmId(), ImageID: v.GetImageId(), Profile: v.GetProfile(), State: vmlib.VMState(v.GetState()),
		Labels: v.GetLabels(), Retained: v.GetRetained(), RetainedReason: v.GetRetainedReason()}
}

func facadeImageToPB(i vmlib.Image) *pb.Image {
	return &pb.Image{Id: i.ID, SizeBytes: i.SizeBytes, Sha256: i.SHA256, Base: i.Base}
}

func facadeImageFromPB(i *pb.Image) vmlib.Image {
	return vmlib.Image{ID: i.GetId(), SizeBytes: i.GetSizeBytes(), SHA256: i.GetSha256(), Base: i.GetBase()}
}

func sshToPB(s vmlib.SSHInfo) *pb.SSHInfo {
	out := &pb.SSHInfo{Host: s.Host, Port: uint32(s.Port), User: s.User, AuthMode: s.AuthMode, KeyReference: s.KeyReference,
		PublicKeyProvisioned: s.PublicKeyProvisioned, HostKeyPolicy: s.HostKeyPolicy, HostKeyFingerprint: s.HostKeyFingerprint, Ready: s.Ready}
	if s.Bastion != nil {
		out.Bastion = &pb.Bastion{Host: s.Bastion.Host, Port: uint32(s.Bastion.Port), User: s.Bastion.User}
	}
	return out
}

func sshFromPB(s *pb.SSHInfo) vmlib.SSHInfo {
	out := vmlib.SSHInfo{Host: s.GetHost(), Port: int(s.GetPort()), User: s.GetUser(), AuthMode: s.GetAuthMode(), KeyReference: s.GetKeyReference(),
		PublicKeyProvisioned: s.GetPublicKeyProvisioned(), HostKeyPolicy: s.GetHostKeyPolicy(), HostKeyFingerprint: s.GetHostKeyFingerprint(), Ready: s.GetReady()}
	if b := s.GetBastion(); b != nil {
		out.Bastion = &vmlib.Bastion{Host: b.GetHost(), Port: int(b.GetPort()), User: b.GetUser()}
	}
	return out
}

// ---- server -----------------------------------------------------------------

type facadeServer struct {
	pb.UnimplementedVMServiceServer
	l vmlib.Lifecycle
}

// RegisterFacade exposes a lifecycle facade (any vmlib.Lifecycle, normally a
// *vmlib.Service over a local or remote backend).
func RegisterFacade(s grpc.ServiceRegistrar, l vmlib.Lifecycle) {
	pb.RegisterVMServiceServer(s, &facadeServer{l: l})
}

func (s *facadeServer) CreateVM(ctx context.Context, r *pb.CreateVMRequest) (*pb.VM, error) {
	if r.GetVmId() != "" {
		if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
			return nil, toStatus(err)
		}
	}
	for _, c := range [][2]string{{"image", r.GetImageId()}, {"profile", r.GetProfile()}} {
		if err := vmlib.ValidateID(c[0], c[1]); err != nil {
			return nil, toStatus(err)
		}
	}
	if err := vmlib.ValidatePublicKey(r.GetSshPublicKey()); err != nil {
		return nil, toStatus(err)
	}
	if len(r.GetLabels()) > 32 {
		return nil, toStatus(vmlib.ErrInvalidArgument)
	}
	// The client's RPC context bounds this call only; Service never ties a
	// running VM's lifetime to it.
	vm, err := s.l.Create(ctx, vmlib.CreateRequest{ID: r.GetVmId(), ImageID: r.GetImageId(), Profile: r.GetProfile(),
		Labels: r.GetLabels(), SSHPublicKey: r.GetSshPublicKey()})
	if err != nil {
		return nil, toStatus(err)
	}
	return vmToPB(vm), nil
}

func (s *facadeServer) vm(ctx context.Context, id string, f func(context.Context, string) (vmlib.VM, error)) (*pb.VM, error) {
	if err := vmlib.ValidateID("vm", id); err != nil {
		return nil, toStatus(err)
	}
	v, err := f(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	return vmToPB(v), nil
}

func (s *facadeServer) GetVM(ctx context.Context, r *pb.VMRef) (*pb.VM, error) {
	return s.vm(ctx, r.GetVmId(), s.l.Get)
}
func (s *facadeServer) StopVM(ctx context.Context, r *pb.VMRef) (*pb.VM, error) {
	return s.vm(ctx, r.GetVmId(), s.l.Stop)
}

func (s *facadeServer) ListVMs(ctx context.Context, _ *pb.ListVMsRequest) (*pb.ListVMsResponse, error) {
	vms, err := s.l.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListVMsResponse{}
	for _, v := range vms {
		out.Vms = append(out.Vms, vmToPB(v))
	}
	return out, nil
}

func (s *facadeServer) GetSSHInfo(ctx context.Context, r *pb.VMRef) (*pb.SSHInfo, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	i, err := s.l.SSHInfo(ctx, r.GetVmId())
	if err != nil {
		return nil, toStatus(err)
	}
	return sshToPB(i), nil
}

func (s *facadeServer) CommitVM(ctx context.Context, r *pb.CommitVMRequest) (*pb.Image, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	if err := vmlib.ValidateID("image", r.GetNewImageId()); err != nil {
		return nil, toStatus(err)
	}
	i, err := s.l.Commit(ctx, r.GetVmId(), r.GetNewImageId())
	if err != nil {
		return nil, toStatus(err)
	}
	return facadeImageToPB(i), nil
}

func (s *facadeServer) DeleteVM(ctx context.Context, r *pb.VMRef) (*pb.DeleteVMResponse, error) {
	if err := vmlib.ValidateID("vm", r.GetVmId()); err != nil {
		return nil, toStatus(err)
	}
	res, err := s.l.Delete(ctx, r.GetVmId())
	if err != nil {
		// A retained disk is reported with its explicit error code.
		return nil, toStatus(err)
	}
	return &pb.DeleteVMResponse{Deleted: res.Deleted, Retained: res.Retained, Detail: res.Detail}, nil
}

func (s *facadeServer) ListImages(ctx context.Context, _ *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	imgs, err := s.l.ListImages(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListImagesResponse{}
	for _, i := range imgs {
		out.Images = append(out.Images, facadeImageToPB(i))
	}
	return out, nil
}

// ---- client -----------------------------------------------------------------

// FacadeClient implements vmlib.Lifecycle over generated gRPC+mTLS. It needs no
// KVM, Firecracker SDK or host access.
type FacadeClient struct {
	conn *grpc.ClientConn
	c    pb.VMServiceClient

	mu      sync.Mutex
	keyRefs map[string]string // client-local key labels, never sent to the server
}

func (c *FacadeClient) remember(id, ref string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keyRefs == nil {
		c.keyRefs = map[string]string{}
	}
	c.keyRefs[id] = ref
}
func (c *FacadeClient) keyRef(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keyRefs[id]
}
func (c *FacadeClient) forget(id string) { c.mu.Lock(); defer c.mu.Unlock(); delete(c.keyRefs, id) }

var _ vmlib.Lifecycle = (*FacadeClient)(nil)

func DialFacade(addr string, cfg *tls.Config) (*FacadeClient, error) {
	opts, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.Dial(addr, opts...)
	if err != nil {
		return nil, err
	}
	return &FacadeClient{conn: conn, c: pb.NewVMServiceClient(conn)}, nil
}

func (c *FacadeClient) Close() error { return c.conn.Close() }

func (c *FacadeClient) vm(r *pb.VM, err error) (vmlib.VM, error) {
	if err != nil {
		return vmlib.VM{}, fromStatus(err)
	}
	return vmFromPB(r), nil
}

func (c *FacadeClient) Create(ctx context.Context, req vmlib.CreateRequest) (vmlib.VM, error) {
	// SSHKeyReference stays client-side metadata and is re-attached below; the
	// server never learns a client-local key label.
	vm, err := c.vm(c.c.CreateVM(ctx, &pb.CreateVMRequest{VmId: req.ID, ImageId: req.ImageID, Profile: req.Profile,
		Labels: req.Labels, SshPublicKey: req.SSHPublicKey}))
	if err == nil && req.SSHKeyReference != "" {
		c.remember(vm.ID, req.SSHKeyReference)
	}
	return vm, err
}

func (c *FacadeClient) Get(ctx context.Context, id string) (vmlib.VM, error) {
	return c.vm(c.c.GetVM(ctx, &pb.VMRef{VmId: id}))
}
func (c *FacadeClient) Stop(ctx context.Context, id string) (vmlib.VM, error) {
	return c.vm(c.c.StopVM(ctx, &pb.VMRef{VmId: id}))
}

func (c *FacadeClient) List(ctx context.Context) ([]vmlib.VM, error) {
	r, err := c.c.ListVMs(ctx, &pb.ListVMsRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.VM
	for _, v := range r.GetVms() {
		out = append(out, vmFromPB(v))
	}
	return out, nil
}

func (c *FacadeClient) SSHInfo(ctx context.Context, id string) (vmlib.SSHInfo, error) {
	r, err := c.c.GetSSHInfo(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		return vmlib.SSHInfo{}, fromStatus(err)
	}
	info := sshFromPB(r)
	if info.KeyReference == "" {
		info.KeyReference = c.keyRef(id)
	}
	return info, nil
}

func (c *FacadeClient) Commit(ctx context.Context, id, newID string) (vmlib.Image, error) {
	r, err := c.c.CommitVM(ctx, &pb.CommitVMRequest{VmId: id, NewImageId: newID})
	if err != nil {
		return vmlib.Image{}, fromStatus(err)
	}
	return facadeImageFromPB(r), nil
}

func (c *FacadeClient) Delete(ctx context.Context, id string) (vmlib.DeleteResult, error) {
	r, err := c.c.DeleteVM(ctx, &pb.VMRef{VmId: id})
	if err != nil {
		return vmlib.DeleteResult{}, fromStatus(err)
	}
	c.forget(id)
	return vmlib.DeleteResult{Deleted: r.GetDeleted(), Retained: r.GetRetained(), Detail: r.GetDetail()}, nil
}

func (c *FacadeClient) ListImages(ctx context.Context) ([]vmlib.Image, error) {
	r, err := c.c.ListImages(ctx, &pb.ListImagesRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	var out []vmlib.Image
	for _, i := range r.GetImages() {
		out = append(out, facadeImageFromPB(i))
	}
	return out, nil
}
